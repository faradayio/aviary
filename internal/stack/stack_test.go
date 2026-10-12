package stack

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

type recordingRunner struct {
	rec *recorder
	c   compose.Stack
}

func (r recordingRunner) Run(args ...string) error {
	r.rec.calls = append(r.rec.calls, r.c.Args(args...))
	if r.rec.fail != nil {
		return r.rec.fail(args)
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
		runner:    func(c compose.Stack) Runner { return recordingRunner{rec, c} },
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
	entries, err := s.registry.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Project == s.Workspace.Project() {
			return e
		}
	}
	t.Fatalf("%s not in the registry", s.Workspace.Project())
	return registry.Entry{}
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

	_, err := load(dir, "", reg)
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

func TestLoadAllocatesPortsForAnInTreeDefinition(t *testing.T) {
	root := tempDir(t)
	writeDefinition(t, root, "compose: {files: [compose.yaml]}\nports: [postgres, redis]\nrequire_env: [AVIARY_TEST_TOKEN]\n")
	t.Setenv("AVIARY_TEST_TOKEN", "token")
	sub := filepath.Join(root, "src", "app")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	s, err := load(sub, "", testRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	if s.Workspace.Root != root || s.Workspace.External {
		t.Errorf("Workspace = %+v, want in-tree root %s", s.Workspace, root)
	}
	if got := s.Entry.Ports; got["postgres"] != registry.FirstPort || got["redis"] != registry.FirstPort+1 {
		t.Errorf("Ports = %v, want postgres and redis from %d", got, registry.FirstPort)
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

	s, err := load(repo, def, testRegistry(t))
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

func TestEnvNamesTheStackAndEveryPort(t *testing.T) {
	s, _ := newStack(t, &config.Config{
		Compose: config.Compose{Files: []string{"compose.yaml"}},
		Ports:   []string{"postgres", "redis", "nick_https"},
	})
	env := s.Env()
	want := map[string]string{
		"COMPOSE_PROJECT_NAME":   "aviary-wt-abc123",
		"AVIARY_PROJECT":         "aviary-wt-abc123",
		"AVIARY_SLUG":            "wt-abc123",
		"AVIARY_ROOT":            "/work/wt",
		"AVIARY_PORT_POSTGRES":   "20000",
		"AVIARY_PORT_REDIS":      "20001",
		"AVIARY_PORT_NICK_HTTPS": "20002",
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
	if !slices.IsSorted(lines) || !slices.Contains(lines, "AVIARY_PORT_REDIS=20001") {
		t.Errorf("EnvLines = %q, want sorted KEY=VALUE lines", lines)
	}
	if c := s.Compose(); c.Env["AVIARY_PORT_NICK_HTTPS"] != "20002" {
		t.Errorf("Compose().Env = %v, want the stack's Env", c.Env)
	}
}

func initConfig() *config.Config {
	return &config.Config{
		Compose: config.Compose{Files: []string{"compose.yaml"}},
		Ports:   []string{"postgres"},
		Init: []config.Step{
			{Service: "diamond", Command: []string{"rake", "db:reset"}},
			{Service: "vannevar", Command: []string{"bin/vannevar", "seed"}},
			{Service: "diamond", Command: []string{"psql", "-f", "billing.sql"}},
		},
	}
}

func TestInitRunsStepsInOrderThenMarksInitialized(t *testing.T) {
	s, rec := newStack(t, initConfig())

	if err := s.Init(false); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, rec,
		argv(nil, "run", "--rm", "diamond", "rake", "db:reset"),
		argv(nil, "run", "--rm", "vannevar", "bin/vannevar", "seed"),
		argv(nil, "run", "--rm", "diamond", "psql", "-f", "billing.sql"),
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

func TestInitStopsAtTheFailingStepAndStaysUninitialized(t *testing.T) {
	s, rec := newStack(t, initConfig())
	rec.fail = func(args []string) error {
		if slices.Contains(args, "vannevar") {
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
	if !strings.Contains(err.Error(), "init step 2 (vannevar bin/vannevar seed)") {
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
		"sojourner": {Service: "sojourner", Needs: []string{"postgres", "redis"}, Command: []string{"pnpm", "run", "test:ci"}},
		"acceptance": {
			Service:  "acceptance",
			Profiles: []string{"acceptance"},
			Needs:    []string{"nick"},
			Command:  []string{"pnpm", "test"},
			Init:     &noInit,
		},
		"popsicle": {Service: "popsicle", Command: []string{"pnpm", "run", "test:ci"}, Init: &noInit},
	}
	return cfg
}

func TestTestRunsInitThenNeedsThenTheSuite(t *testing.T) {
	s, rec := newStack(t, suiteConfig())

	if err := s.Test("sojourner", []string{"--", "--grep", "users"}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, rec,
		argv([]string{"dev"}, "run", "--rm", "diamond", "rake", "db:reset"),
		argv([]string{"dev"}, "up", "--detach", "--wait", "postgres", "redis"),
		argv([]string{"dev"}, "run", "--rm", "sojourner", "pnpm", "run", "test:ci", "--grep", "users"),
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
		argv(profiles, "up", "--detach", "--wait", "nick"),
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

	if err := s.Test("popsicle", nil); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, rec, argv([]string{"dev"}, "run", "--rm", "popsicle", "pnpm", "run", "test:ci"))
}

func TestTestPropagatesTheSuiteExitCode(t *testing.T) {
	s, rec := newStack(t, suiteConfig())
	rec.fail = func(args []string) error {
		if slices.Contains(args, "popsicle") {
			return &compose.ExitError{Code: 3}
		}
		return nil
	}

	err := s.Test("popsicle", nil)
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
	if err == nil || !strings.Contains(err.Error(), "nick") || !strings.Contains(err.Error(), "acceptance") {
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
	if err == nil || !strings.Contains(err.Error(), "acceptance, popsicle, sojourner") {
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
