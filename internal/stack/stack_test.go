package stack

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/faradayio/aviary/internal/compose"
	"github.com/faradayio/aviary/internal/config"
	"github.com/faradayio/aviary/internal/registry"
	"github.com/faradayio/aviary/internal/workspace"
)

// allFree is a Prober that hands out every port, so tests never bind sockets.
type allFree struct{}

func (allFree) Free(int) bool { return true }

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.OpenAt(filepath.Join(t.TempDir(), "registry.json"))
	reg.Prober = allFree{}
	return reg
}

// recorder captures the full docker argv of every compose call, including
// project, profiles and files, and fails a call when fail says so.
type recorder struct {
	calls [][]string
	fail  func(args []string) error
}

// run is a Stack runner that records the call instead of running it.
func (r *recorder) run(c compose.Stack, args ...string) error {
	r.calls = append(r.calls, c.Args(args...))
	if r.fail != nil {
		return r.fail(args)
	}
	return nil
}

// newStack builds a stack for cfg at /work/wt with ports allocated in a temp
// registry, recording compose calls instead of running them.
func newStack(t *testing.T, cfg *config.Config) (*Stack, *recorder) {
	t.Helper()
	reg := testRegistry(t)
	ws := workspace.Workspace{Root: "/work/wt", Definition: "/work/wt", Slug: "wt-abc123"}
	entry, err := reg.Allocate(ws.Project(), ws.Root, "", cfg.Ports)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	s := &Stack{
		Config:    cfg,
		Workspace: ws,
		Entry:     entry,
		registry:  reg,
		runner:    rec.run,
		stderr:    io.Discard,
	}
	return s, rec
}

// argv is the docker argv for /work/wt's stack with compose.yaml, the given
// profiles and the subcommand.
func argv(profiles []string, sub ...string) []string {
	out := []string{"compose", "--project-name", "aviary-wt-abc123", "--project-directory", "/work/wt"}
	for _, p := range profiles {
		out = append(out, "--profile", p)
	}
	out = append(out, "--file", "/work/wt/compose.yaml")
	return append(out, sub...)
}

func assertCalls(t *testing.T, rec *recorder, want ...[]string) {
	t.Helper()
	if len(rec.calls) != len(want) {
		t.Fatalf("got %d compose calls, want %d:\n got %q\nwant %q", len(rec.calls), len(want), rec.calls, want)
	}
	for i := range want {
		if !slices.Equal(rec.calls[i], want[i]) {
			t.Errorf("call %d:\n got %q\nwant %q", i, rec.calls[i], want[i])
		}
	}
}

func entryFor(t *testing.T, s *Stack) registry.Entry {
	t.Helper()
	e, ok, err := s.registry.Get(s.Workspace.Project())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("%s not in the registry", s.Workspace.Project())
	}
	return e
}

func writeDefinition(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// tempDir returns a fresh temp directory with symlinks resolved, matching
// the paths workspace.Detect and git report.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// unset removes name from the environment for the rest of the test.
func unset(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	os.Unsetenv(name)
}

func TestLoadReportsEveryMissingRequiredEnvBeforeRegistering(t *testing.T) {
	dir := tempDir(t)
	writeDefinition(t, dir, `
compose: {files: [compose.yaml]}
ports: [postgres]
require_env: [AVIARY_TEST_SET, AVIARY_TEST_MISSING_A, AVIARY_TEST_EMPTY, AVIARY_TEST_MISSING_B]
`)
	t.Setenv("AVIARY_TEST_SET", "token")
	t.Setenv("AVIARY_TEST_EMPTY", "")
	unset(t, "AVIARY_TEST_MISSING_A")
	unset(t, "AVIARY_TEST_MISSING_B")
	reg := testRegistry(t)

	_, err := Load(dir, "", Options{Registry: reg})
	if err == nil {
		t.Fatal("expected an error for missing require_env names")
	}
	msg := err.Error()
	for _, want := range []string{"AVIARY_TEST_MISSING_A", "AVIARY_TEST_EMPTY", "AVIARY_TEST_MISSING_B", filepath.Join(dir, config.FileName)} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, "AVIARY_TEST_SET") {
		t.Errorf("error %q names a variable that is set", msg)
	}
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("registry has %d entries after a failed load, want 0", len(entries))
	}
}

func TestLoadSkipRequireEnvLoadsWithMissingNames(t *testing.T) {
	dir := tempDir(t)
	writeDefinition(t, dir, "compose: {files: [compose.yaml]}\nports: [postgres]\nrequire_env: [AVIARY_TEST_MISSING_A]\n")
	unset(t, "AVIARY_TEST_MISSING_A")

	s, err := Load(dir, "", Options{Registry: testRegistry(t), SkipRequireEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	if base := registry.BasePort(s.Workspace.Project()); s.Entry.Ports["postgres"] != base {
		t.Errorf("Ports = %v, want postgres at %d", s.Entry.Ports, base)
	}
}

func TestLoadAllocatesPortsForAnInTreeDefinition(t *testing.T) {
	root := tempDir(t)
	writeDefinition(t, root, "compose: {files: [compose.yaml]}\nports: [postgres, redis]\nrequire_env: [AVIARY_TEST_TOKEN]\n")
	t.Setenv("AVIARY_TEST_TOKEN", "token")
	sub := filepath.Join(root, "src", "app")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	s, err := Load(sub, "", Options{Registry: testRegistry(t)})
	if err != nil {
		t.Fatal(err)
	}
	if s.Workspace.Root != root || s.Workspace.External {
		t.Errorf("Workspace = %+v, want in-tree root %s", s.Workspace, root)
	}
	base := registry.BasePort(s.Workspace.Project())
	if got := s.Entry.Ports; got["postgres"] != base || got["redis"] != base+1 {
		t.Errorf("Ports = %v, want postgres and redis from %d", got, base)
	}
	if e := entryFor(t, s); e.Config != "" || e.Root != root {
		t.Errorf("registry entry = %+v, want root %s and empty config for an in-tree definition", e, root)
	}
}

func TestLoadRecordsAnExternalDefinitionInTheRegistry(t *testing.T) {
	repo := tempDir(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(repo))
	if out, err := exec.Command("git", "-C", repo, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	def := tempDir(t)
	writeDefinition(t, def, "compose: {files: [compose.yaml]}\nports: [postgres]\n")

	s, err := Load(repo, def, Options{Registry: testRegistry(t)})
	if err != nil {
		t.Fatal(err)
	}
	e := entryFor(t, s)
	if e.Config != def || e.Root != repo {
		t.Errorf("registry entry = %+v, want root %s and config %s", e, repo, def)
	}
	if c := s.Compose(); c.Root != repo || c.Definition != def {
		t.Errorf("Compose() root %s definition %s, want %s and %s", c.Root, c.Definition, repo, def)
	}
}

func TestLoadNoAllocateLeavesAnUnregisteredStackUnregistered(t *testing.T) {
	root := tempDir(t)
	writeDefinition(t, root, "compose: {files: [compose.yaml]}\nports: [postgres]\n")
	reg := testRegistry(t)

	s, err := Load(root, "", Options{Registry: reg, NoAllocate: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Entry.Ports) != 0 {
		t.Errorf("Ports = %v, want none for a stack that was never registered", s.Entry.Ports)
	}
	if _, ok := s.Env()["AVIARY_PORT_POSTGRES"]; ok {
		t.Errorf("Env = %v, want no port variables so compose falls back to its defaults", s.Env())
	}
	if _, err := os.Stat(reg.Path()); !os.IsNotExist(err) {
		t.Errorf("Load with NoAllocate wrote %s (stat err %v)", reg.Path(), err)
	}
}

func TestLoadNoAllocateUsesTheRegisteredPortsWithoutWriting(t *testing.T) {
	root := tempDir(t)
	writeDefinition(t, root, "compose: {files: [compose.yaml]}\nports: [postgres]\n")
	reg := testRegistry(t)
	if _, err := Load(root, "", Options{Registry: reg}); err != nil {
		t.Fatal(err)
	}
	// A port name added since registration would be allocated by a plain
	// Load; NoAllocate must leave the registry as it is.
	writeDefinition(t, root, "compose: {files: [compose.yaml]}\nports: [postgres, redis]\n")
	before, err := os.ReadFile(reg.Path())
	if err != nil {
		t.Fatal(err)
	}

	s, err := Load(root, "", Options{Registry: reg, NoAllocate: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"postgres": registry.BasePort(s.Workspace.Project())}; !maps.Equal(s.Entry.Ports, want) {
		t.Errorf("Ports = %v, want the registered %v", s.Entry.Ports, want)
	}
	after, err := os.ReadFile(reg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("Load with NoAllocate changed the registry:\n%s\nwas\n%s", after, before)
	}
}

func TestDownVolumesOnAnUnregisteredStackDoesNotRegisterIt(t *testing.T) {
	root := tempDir(t)
	writeDefinition(t, root, "compose: {files: [compose.yaml]}\nports: [postgres]\n")
	reg := testRegistry(t)
	s, err := Load(root, "", Options{Registry: reg, NoAllocate: true})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	s.runner = rec.run

	if err := s.Down(true); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 {
		t.Errorf("Down made %d compose calls, want 1", len(rec.calls))
	}
	if _, err := os.Stat(reg.Path()); !os.IsNotExist(err) {
		t.Errorf("Down --volumes on an unregistered stack wrote %s (stat err %v)", reg.Path(), err)
	}
}

func TestEnvNamesTheStackAndEveryPort(t *testing.T) {
	s, _ := newStack(t, &config.Config{
		Compose: config.Compose{Files: []string{"compose.yaml"}},
		Ports:   []string{"postgres", "redis", "web_https"},
	})
	env := s.Env()
	base := registry.BasePort("aviary-wt-abc123")
	port := func(i int) string { return strconv.Itoa(base + i) }
	want := map[string]string{
		"COMPOSE_PROJECT_NAME":  "aviary-wt-abc123",
		"AVIARY_PROJECT":        "aviary-wt-abc123",
		"AVIARY_SLUG":           "wt-abc123",
		"AVIARY_ROOT":           "/work/wt",
		"AVIARY_CONFIG_DIR":     "/work/wt",
		"AVIARY_PORT_POSTGRES":  port(0),
		"AVIARY_PORT_REDIS":     port(1),
		"AVIARY_PORT_WEB_HTTPS": port(2),
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("Env[%s] = %q, want %q", k, env[k], v)
		}
	}
	if len(env) != len(want) {
		t.Errorf("Env has %d keys, want %d: %v", len(env), len(want), env)
	}
	lines := s.EnvLines()
	if !slices.IsSorted(lines) || !slices.Contains(lines, "AVIARY_PORT_REDIS="+port(1)) {
		t.Errorf("EnvLines = %q, want sorted KEY=VALUE lines", lines)
	}
	if c := s.Compose(); c.Env["AVIARY_PORT_WEB_HTTPS"] != port(2) {
		t.Errorf("Compose().Env = %v, want the stack's Env", c.Env)
	}
}

func TestExportLinesSingleQuoteEveryValue(t *testing.T) {
	s, _ := newStack(t, &config.Config{
		Compose: config.Compose{Files: []string{"compose.yaml"}},
		Ports:   []string{"postgres"},
	})
	s.Workspace.Root = "/work/a b$HOME'c(d)"
	lines := s.ExportLines()
	if !slices.IsSorted(lines) {
		t.Errorf("ExportLines = %q, want sorted lines", lines)
	}
	for _, want := range []string{
		"export AVIARY_PORT_POSTGRES='" + strconv.Itoa(registry.BasePort("aviary-wt-abc123")) + "'",
		`export AVIARY_ROOT='/work/a b$HOME'\''c(d)'`,
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("ExportLines = %q, want a line %s", lines, want)
		}
	}

	// A shell that evals the lines must see the root byte for byte.
	script := strings.Join(lines, "\n") + "\nprintf %s \"$AVIARY_ROOT\""
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("sh -c on the export lines: %v", err)
	}
	if string(out) != s.Workspace.Root {
		t.Errorf("AVIARY_ROOT after eval = %q, want %q", out, s.Workspace.Root)
	}
}

func initConfig() *config.Config {
	return &config.Config{
		Compose: config.Compose{Files: []string{"compose.yaml"}},
		Ports:   []string{"postgres"},
		Init: []config.Step{
			{Service: "api", Command: []string{"rake", "db:reset"}},
			{Service: "worker", Command: []string{"bin/worker", "seed"}},
			{Service: "api", Command: []string{"psql", "-f", "reference.sql"}},
		},
	}
}

func TestInitRunsStepsInOrderThenMarksInitialized(t *testing.T) {
	s, rec := newStack(t, initConfig())

	if err := s.Init(false); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, rec,
		argv(nil, "run", "--rm", "api", "rake", "db:reset"),
		argv(nil, "run", "--rm", "worker", "bin/worker", "seed"),
		argv(nil, "run", "--rm", "api", "psql", "-f", "reference.sql"),
	)
	if s.Entry.InitializedAt == nil || entryFor(t, s).InitializedAt == nil {
		t.Fatal("stack not marked initialized after init")
	}

	rec.calls = nil
	if err := s.Init(false); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, rec)

	if err := s.Init(true); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 3 {
		t.Errorf("Init(true) made %d compose calls, want the 3 steps again", len(rec.calls))
	}
}

func TestInitReportsEachStepsTimeAndTheTotal(t *testing.T) {
	s, _ := newStack(t, initConfig())
	var stderr bytes.Buffer
	s.stderr = &stderr

	if err := s.Init(false); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`aviary: init: api rake db:reset`,
		`aviary: init: step 1/3 ok \d+\.\ds`,
		`aviary: init: worker bin/worker seed`,
		`aviary: init: step 2/3 ok \d+\.\ds`,
		`aviary: init: api psql -f reference.sql`,
		`aviary: init: step 3/3 ok \d+\.\ds`,
		`aviary: init: total \d+\.\ds`,
	}
	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("init printed %d lines, want %d:\n%s", len(lines), len(want), stderr.String())
	}
	for i, pattern := range want {
		if !regexp.MustCompile("^" + pattern + "$").MatchString(lines[i]) {
			t.Errorf("line %d = %q, want it to match %q", i+1, lines[i], pattern)
		}
	}
}

func TestInitReportsNoTimesAfterAFailingStep(t *testing.T) {
	s, rec := newStack(t, initConfig())
	var stderr bytes.Buffer
	s.stderr = &stderr
	rec.fail = func(args []string) error {
		if slices.Contains(args, "worker") {
			return &compose.ExitError{Code: 2}
		}
		return nil
	}

	if err := s.Init(false); err == nil {
		t.Fatal("expected the failing step to fail Init")
	}
	if got := stderr.String(); strings.Contains(got, "step 2/3 ok") || strings.Contains(got, "total") {
		t.Errorf("init reported success past the failing step:\n%s", got)
	}
}

func TestInitWritesTheRegistryOnceAfterTheSteps(t *testing.T) {
	s, rec := newStack(t, initConfig())
	before := entryFor(t, s)
	raw, err := os.ReadFile(s.registry.Path())
	if err != nil {
		t.Fatal(err)
	}
	rec.fail = func([]string) error {
		if now, err := os.ReadFile(s.registry.Path()); err != nil || string(now) != string(raw) {
			t.Errorf("registry changed while an init step ran (err %v)", err)
		}
		return nil
	}

	if err := s.Init(false); err != nil {
		t.Fatal(err)
	}
	after := entryFor(t, s)
	if after.InitializedAt == nil {
		t.Fatal("initialized_at not set after init")
	}
	if !after.CreatedAt.Equal(before.CreatedAt) || !maps.Equal(after.Ports, before.Ports) || after.Root != before.Root {
		t.Errorf("entry after init = %+v, want %+v with only initialized_at added", after, before)
	}
	var file struct {
		Version int `json:"version"`
	}
	if raw, err = os.ReadFile(s.registry.Path()); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &file); err != nil || file.Version != registry.Version {
		t.Errorf("registry version after init = %d (err %v), want %d", file.Version, err, registry.Version)
	}
	if !reflect.DeepEqual(s.Entry, after) {
		t.Errorf("Stack.Entry = %+v, want the stored entry %+v", s.Entry, after)
	}
}

func TestInitStopsAtTheFailingStepAndStaysUninitialized(t *testing.T) {
	s, rec := newStack(t, initConfig())
	rec.fail = func(args []string) error {
		if slices.Contains(args, "worker") {
			return &compose.ExitError{Code: 2}
		}
		return nil
	}

	err := s.Init(false)
	if err == nil {
		t.Fatal("expected the failing step to fail Init")
	}
	if len(rec.calls) != 2 {
		t.Errorf("made %d compose calls, want 2: steps after the failure must not run", len(rec.calls))
	}
	if !strings.Contains(err.Error(), "init step 2 (worker bin/worker seed)") {
		t.Errorf("error %q does not name the failing step", err)
	}
	var exit *compose.ExitError
	if errors.As(err, &exit) {
		t.Errorf("error %q unwraps to an ExitError; an init failure is aviary's own error", err)
	}
	if s.Entry.InitializedAt != nil || entryFor(t, s).InitializedAt != nil {
		t.Error("stack marked initialized after a failed step")
	}
}

func suiteConfig() *config.Config {
	cfg := initConfig()
	cfg.Init = cfg.Init[:1]
	cfg.Compose.Profiles = []string{"dev"}
	noInit := false
	cfg.Suites = map[string]config.Suite{
		"web": {Service: "web", Needs: []string{"postgres", "redis"}, Command: []string{"pnpm", "run", "test:ci"}},
		"acceptance": {
			Service:  "acceptance",
			Profiles: []string{"acceptance"},
			Needs:    []string{"proxy"},
			Command:  []string{"pnpm", "test"},
			Init:     &noInit,
		},
		"frontend": {Service: "frontend", Command: []string{"pnpm", "run", "test:ci"}, Init: &noInit},
	}
	return cfg
}

func TestTestRunsInitThenNeedsThenTheSuite(t *testing.T) {
	s, rec := newStack(t, suiteConfig())

	if err := s.Test("web", []string{"--", "--grep", "users"}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, rec,
		argv([]string{"dev"}, "run", "--rm", "api", "rake", "db:reset"),
		argv([]string{"dev"}, "up", "--detach", "--wait", "postgres", "redis"),
		argv([]string{"dev"}, "run", "--rm", "web", "pnpm", "run", "test:ci", "--grep", "users"),
	)
	if entryFor(t, s).InitializedAt == nil {
		t.Error("stack not marked initialized by a suite that wants init")
	}
}

func TestTestPassesStackAndSuiteProfilesAndSkipsInitWhenTheSuiteSaysSo(t *testing.T) {
	s, rec := newStack(t, suiteConfig())

	if err := s.Test("acceptance", []string{"--", "spec/login.ts"}); err != nil {
		t.Fatal(err)
	}
	profiles := []string{"dev", "acceptance"}
	assertCalls(t, rec,
		argv(profiles, "up", "--detach", "--wait", "proxy"),
		argv(profiles, "run", "--rm", "acceptance", "pnpm", "test", "spec/login.ts"),
	)
	if entryFor(t, s).InitializedAt != nil {
		t.Error("a suite with init: false marked the stack initialized")
	}
	if got := s.Compose().Profiles; !slices.Equal(got, []string{"dev"}) {
		t.Errorf("stack-wide profiles became %q after a suite run", got)
	}
}

func TestTestWithoutNeedsOnlyRunsTheSuite(t *testing.T) {
	s, rec := newStack(t, suiteConfig())

	if err := s.Test("frontend", nil); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, rec, argv([]string{"dev"}, "run", "--rm", "frontend", "pnpm", "run", "test:ci"))
}

func TestTestPropagatesTheSuiteExitCode(t *testing.T) {
	s, rec := newStack(t, suiteConfig())
	rec.fail = func(args []string) error {
		if slices.Contains(args, "frontend") {
			return &compose.ExitError{Code: 3}
		}
		return nil
	}

	err := s.Test("frontend", nil)
	var exit *compose.ExitError
	if !errors.As(err, &exit) || exit.Code != 3 {
		t.Fatalf("Test error = %v, want *compose.ExitError with code 3", err)
	}
}

func TestTestFailingNeedsIsAviarysOwnError(t *testing.T) {
	s, rec := newStack(t, suiteConfig())
	rec.fail = func(args []string) error {
		if args[0] == "up" {
			return &compose.ExitError{Code: 1}
		}
		return nil
	}

	err := s.Test("acceptance", nil)
	if err == nil || !strings.Contains(err.Error(), "proxy") || !strings.Contains(err.Error(), "acceptance") {
		t.Fatalf("Test error = %v, want one naming the needs and the suite", err)
	}
	var exit *compose.ExitError
	if errors.As(err, &exit) {
		t.Errorf("error %q unwraps to an ExitError; only the suite's own status passes through", err)
	}
	if len(rec.calls) != 1 {
		t.Errorf("made %d compose calls, want the suite not to run after its needs failed", len(rec.calls))
	}
}

func TestTestUnknownSuiteListsTheSuites(t *testing.T) {
	s, rec := newStack(t, suiteConfig())

	err := s.Test("nope", nil)
	if err == nil || !strings.Contains(err.Error(), "acceptance, frontend, web") {
		t.Fatalf("Test error = %v, want the sorted suite names", err)
	}
	assertCalls(t, rec)
}

func TestDownKeepsInitializedUnlessVolumesGo(t *testing.T) {
	s, rec := newStack(t, initConfig())
	if err := s.Init(false); err != nil {
		t.Fatal(err)
	}
	rec.calls = nil

	if err := s.Down(false); err != nil {
		t.Fatal(err)
	}
	if entryFor(t, s).InitializedAt == nil {
		t.Error("down without volumes cleared initialized")
	}
	if err := s.Down(true); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, rec,
		argv(nil, "down", "--remove-orphans"),
		argv(nil, "down", "--remove-orphans", "--volumes"),
	)
	if s.Entry.InitializedAt != nil || entryFor(t, s).InitializedAt != nil {
		t.Error("down --volumes left the stack marked initialized")
	}
}
