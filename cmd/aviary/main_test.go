package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/faradayio/aviary/internal/compose"
	"github.com/faradayio/aviary/internal/config"
	"github.com/faradayio/aviary/internal/registry"
	"github.com/faradayio/aviary/internal/workspace"
)

// allFree is a Prober that hands out every port, so tests never bind sockets.
type allFree struct{}

func (allFree) Free(int) bool { return true }

// harness is an app wired to a temp registry and recording fakes for every
// call that would reach Docker or git.
type harness struct {
	app      *app
	reg      *registry.Registry
	projects []compose.Project
	listErr  error
	downed   []string
	downErr  error
	composed []composeCall
	// goneRoot is the deleted worktree lsFixture registers.
	goneRoot string
	// stderr holds what the last run printed to stderr.
	stderr bytes.Buffer
}

type composeCall struct {
	stack compose.Stack
	args  []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{reg: registry.OpenAt(filepath.Join(t.TempDir(), "registry.json"))}
	h.reg.Prober = allFree{}
	h.app = &app{
		openRegistry: func() (*registry.Registry, error) { return h.reg, nil },
		listProjects: func() ([]compose.Project, error) { return h.projects, h.listErr },
		downProject: func(name string) error {
			h.downed = append(h.downed, name)
			return h.downErr
		},
		runCompose: func(c compose.Stack, args ...string) error {
			h.composed = append(h.composed, composeCall{c, args})
			return nil
		},
		branch: func(root string) string { return "branch-of-" + filepath.Base(root) },
	}
	return h
}

// run executes the CLI with args and returns what it printed to stdout;
// stderr goes to h.stderr.
func (h *harness) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := h.app.rootCmd()
	var out bytes.Buffer
	h.stderr.Reset()
	cmd.SetOut(&out)
	cmd.SetErr(&h.stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func (h *harness) register(t *testing.T, project, root, config string, ports ...string) {
	t.Helper()
	if _, err := h.reg.Allocate(project, root, config, ports); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) entry(t *testing.T, project string) registry.Entry {
	t.Helper()
	e, ok, err := h.reg.Get(project)
	if err != nil || !ok {
		t.Fatalf("registry entry %s: found %v, err %v", project, ok, err)
	}
	return e
}

func (h *harness) projectNames(t *testing.T) []string {
	t.Helper()
	entries, err := h.reg.List()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Project)
	}
	return names
}

// basePort is the port a fresh registry hands the first port name of the
// stack rooted at root, as a string.
func basePort(root string) string {
	return strconv.Itoa(registry.BasePort(workspace.Workspace{Slug: workspace.Slug(root)}.Project()))
}

func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeDefinition(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	if out, err := exec.Command("git", "-C", dir, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
}

func TestHelpListsEveryCommand(t *testing.T) {
	out, err := newHarness(t).run(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"up", "down", "test", "init", "run", "exec", "ps", "logs", "build", "compose", "env", "ls", "gc", "rm", "--config"} {
		if !strings.Contains(out, "  "+name+" ") {
			t.Errorf("help does not list %s:\n%s", name, out)
		}
	}
}

func TestVersionPrintsTheLinkedVersion(t *testing.T) {
	out, err := newHarness(t).run(t, "--version")
	if err != nil {
		t.Fatal(err)
	}
	if out != "aviary version dev\n" {
		t.Errorf("aviary --version = %q, want the default dev version", out)
	}

	old := version
	t.Cleanup(func() { version = old })
	version = "1.2.3"
	if out, _ := newHarness(t).run(t, "--version"); out != "aviary version 1.2.3\n" {
		t.Errorf("aviary --version = %q, want the version set at link time", out)
	}

	// The version flag takes -v only on the root: down keeps it for --volumes.
	down, _, err := newHarness(t).app.rootCmd().Find([]string{"down", "-v"})
	if err != nil {
		t.Fatal(err)
	}
	if err := down.ParseFlags([]string{"-v"}); err != nil {
		t.Fatal(err)
	}
	if volumes, _ := down.Flags().GetBool("volumes"); !volumes {
		t.Error("aviary down -v did not set --volumes")
	}
}

func TestTestHelpShowsHelp(t *testing.T) {
	// Help needs no stack, so it runs outside any worktree.
	t.Chdir(tempDir(t))
	for _, flag := range []string{"--help", "-h"} {
		h := newHarness(t)
		out, err := h.run(t, "test", flag)
		if err != nil {
			t.Fatalf("aviary test %s: %v", flag, err)
		}
		for _, want := range []string{"test [suite [-- args...]]", "Run a named test suite from aviary.yml."} {
			if !strings.Contains(out, want) {
				t.Errorf("aviary test %s output lacks %q:\n%s", flag, want, out)
			}
		}
		if len(h.composed) != 0 {
			t.Errorf("aviary test %s ran compose: %v", flag, h.composed)
		}
	}
}

func TestUnknownCommandFails(t *testing.T) {
	_, err := newHarness(t).run(t, "bogus")
	if err == nil || !strings.Contains(err.Error(), `unknown command "bogus"`) {
		t.Errorf("aviary bogus: error = %v, want unknown command", err)
	}
}

// lsFixture registers an initialized in-tree stack whose worktree exists and
// is running, and an external one whose worktree is gone, from a directory
// that is still there, and that compose does not know about.
func lsFixture(t *testing.T) (h *harness, liveRoot string) {
	t.Helper()
	h = newHarness(t)
	liveRoot = tempDir(t)
	h.register(t, "aviary-live-111111", liveRoot, "", "postgres", "redis")
	if _, err := h.reg.SetInitialized("aviary-live-111111", true); err != nil {
		t.Fatal(err)
	}
	h.goneRoot = filepath.Join(tempDir(t), "gone")
	h.register(t, "aviary-gone-222222", h.goneRoot, "/defs/myrepo")
	h.projects = []compose.Project{
		{Name: "aviary-live-111111", Status: "running(2)"},
		{Name: "someone-elses", Status: "running(1)"},
	}
	return h, liveRoot
}

func TestLsJSONJoinsRegistryWithComposeStatus(t *testing.T) {
	h, liveRoot := lsFixture(t)

	out, err := h.run(t, "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("ls --json is not a JSON array: %v\n%s", err, out)
	}
	base := registry.BasePort("aviary-live-111111")
	gone, live := h.entry(t, "aviary-gone-222222"), h.entry(t, "aviary-live-111111")
	want := []map[string]any{
		{
			"project": "aviary-gone-222222", "status": "down", "root": h.goneRoot,
			"branch": "", "worktree_missing": true, "initialized": false,
			"created_at": gone.CreatedAt.Format(time.RFC3339Nano), "initialized_at": nil,
			"ports": map[string]any{}, "config": "/defs/myrepo",
		},
		{
			"project": "aviary-live-111111", "status": "running(2)", "root": liveRoot,
			"branch": "branch-of-" + filepath.Base(liveRoot), "worktree_missing": false, "initialized": true,
			"created_at": live.CreatedAt.Format(time.RFC3339Nano), "initialized_at": live.InitializedAt.Format(time.RFC3339Nano),
			"ports":  map[string]any{"postgres": float64(base), "redis": float64(base + 1)},
			"config": "in-tree",
		},
	}
	for _, row := range rows {
		for _, key := range []string{"created_at", "initialized_at"} {
			if v, ok := row[key].(string); ok {
				if _, err := time.Parse(time.RFC3339, v); err != nil {
					t.Errorf("%s %s = %q is not RFC 3339: %v", row["project"], key, v, err)
				}
			}
		}
	}
	got, _ := json.Marshal(rows)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Errorf("ls --json rows\n got %s\nwant %s", got, exp)
	}
}

func TestLsJSONWithNoStacksIsAnEmptyArray(t *testing.T) {
	out, err := newHarness(t).run(t, "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("ls --json with an empty registry = %q, want []", out)
	}
}

func TestLsTable(t *testing.T) {
	h, liveRoot := lsFixture(t)

	out, err := h.run(t, "ls")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("ls printed %d lines, want header and two rows:\n%s", len(lines), out)
	}
	base := registry.BasePort("aviary-live-111111")
	for i, want := range [][]string{
		{"PROJECT", "STATUS", "BRANCH", "WORKTREE", "CONFIG", "PORTS"},
		{"aviary-gone-222222", "down", "(worktree", "missing)", "-", h.goneRoot, "/defs/myrepo", "-"},
		{"aviary-live-111111", "running(2)", "branch-of-" + filepath.Base(liveRoot), liveRoot, "in-tree", "postgres=" + strconv.Itoa(base), "redis=" + strconv.Itoa(base+1)},
	} {
		if got := strings.Fields(lines[i]); !slices.Equal(got, want) {
			t.Errorf("line %d = %q, want fields %q", i, lines[i], want)
		}
	}
}

func TestLsReportsUnknownStatusWhenComposeLsFails(t *testing.T) {
	h, _ := lsFixture(t)
	h.listErr = errors.New("cannot connect to the Docker daemon")
	warning := "aviary: docker compose ls failed: cannot connect to the Docker daemon; statuses unknown\n"

	out, err := h.run(t, "ls", "--json")
	if err != nil {
		t.Fatalf("ls --json failed with compose ls down: %v", err)
	}
	var rows []stackRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("ls --json is not a JSON array: %v\n%s", err, out)
	}
	if len(rows) != 2 || rows[0].Status != "unknown" || rows[1].Status != "unknown" {
		t.Errorf("ls --json rows = %+v, want both with status unknown", rows)
	}
	if got := h.stderr.String(); got != warning {
		t.Errorf("ls --json stderr = %q, want %q", got, warning)
	}

	out, err = h.run(t, "ls")
	if err != nil {
		t.Fatalf("ls failed with compose ls down: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || strings.Fields(lines[1])[1] != "unknown" || strings.Fields(lines[2])[1] != "unknown" {
		t.Errorf("ls table with compose ls down =\n%s\nwant both rows with status unknown", out)
	}
	if got := h.stderr.String(); got != warning {
		t.Errorf("ls stderr = %q, want %q", got, warning)
	}
}

func TestGCDryRunReportsWithoutRemoving(t *testing.T) {
	h, _ := lsFixture(t)

	out, err := h.run(t, "gc", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if want := "aviary-gone-222222: worktree " + h.goneRoot + " is gone; would remove\n"; out != want {
		t.Errorf("gc --dry-run printed %q, want %q", out, want)
	}
	if len(h.downed) != 0 {
		t.Errorf("gc --dry-run took down %v", h.downed)
	}
	if got := h.projectNames(t); len(got) != 2 {
		t.Errorf("registry after gc --dry-run = %v, want both stacks", got)
	}
}

func TestGCRemovesOnlyStacksWhoseWorktreeIsGone(t *testing.T) {
	h, _ := lsFixture(t)

	if _, err := h.run(t, "gc"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.downed, []string{"aviary-gone-222222"}) {
		t.Errorf("gc took down %v, want only the orphan", h.downed)
	}
	if got := h.projectNames(t); !slices.Equal(got, []string{"aviary-live-111111"}) {
		t.Errorf("registry after gc = %v, want only the live stack", got)
	}
}

func TestRmProjectTakesItDownAndReleasesIt(t *testing.T) {
	h, _ := lsFixture(t)

	if _, err := h.run(t, "rm", "--project", "aviary-live-111111"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.downed, []string{"aviary-live-111111"}) {
		t.Errorf("rm took down %v", h.downed)
	}
	if got := h.projectNames(t); !slices.Equal(got, []string{"aviary-gone-222222"}) {
		t.Errorf("registry after rm = %v, want only the other stack", got)
	}
}

func TestRmProjectKeepsTheEntryWhenDownFails(t *testing.T) {
	h, _ := lsFixture(t)
	h.downErr = &compose.ExitError{Code: 3}

	_, err := h.run(t, "rm", "--project", "aviary-live-111111")
	if err == nil || !strings.Contains(err.Error(), "aviary-live-111111") {
		t.Fatalf("rm error = %v, want one naming the project", err)
	}
	var exit *compose.ExitError
	if errors.As(err, &exit) {
		t.Error("rm passed compose's bare exit status through instead of reporting its own error")
	}
	if got := h.projectNames(t); len(got) != 2 {
		t.Errorf("registry after a failed rm = %v, want both stacks", got)
	}
}

func TestRmUnknownProjectFails(t *testing.T) {
	h, _ := lsFixture(t)

	_, err := h.run(t, "rm", "--project", "aviary-nope-000000")
	if err == nil || !strings.Contains(err.Error(), "aviary-nope-000000") {
		t.Fatalf("rm of an unknown project: error = %v, want one naming it", err)
	}
	if len(h.downed) != 0 {
		t.Errorf("rm of an unknown project took down %v", h.downed)
	}
}

func TestRmWithoutProjectRemovesThisWorktreesStackByLabel(t *testing.T) {
	root := tempDir(t)
	writeDefinition(t, root, "compose: {files: [compose.yaml]}\nports: [postgres]\n")
	t.Chdir(root)
	h := newHarness(t)
	project := envJSON(t, h, "env", "--json")["AVIARY_PROJECT"]
	h.register(t, "aviary-other-333333", "/wt/other", "", "postgres")
	// rm needs only to find aviary.yml, not to parse it.
	writeDefinition(t, root, "not: [valid")

	if _, err := h.run(t, "rm"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.downed, []string{project}) {
		t.Errorf("rm took down %v by label, want [%s]", h.downed, project)
	}
	if len(h.composed) != 0 {
		t.Errorf("rm ran compose through the stack's files: %v", h.composed)
	}
	if got := h.projectNames(t); !slices.Equal(got, []string{"aviary-other-333333"}) {
		t.Errorf("registry after rm = %v, want only the other stack", got)
	}
}

func TestRmWithoutProjectFailsForAnUnregisteredWorktree(t *testing.T) {
	root := tempDir(t)
	writeDefinition(t, root, "compose: {files: [compose.yaml]}\n")
	t.Chdir(root)
	h := newHarness(t)

	_, err := h.run(t, "rm")
	if err == nil || !strings.Contains(err.Error(), workspace.Workspace{Slug: workspace.Slug(root)}.Project()) {
		t.Fatalf("rm in an unregistered worktree: error = %v, want one naming its project", err)
	}
	if len(h.downed) != 0 {
		t.Errorf("rm of an unregistered worktree took down %v", h.downed)
	}
}

func TestInspectingCommandsDoNotRegisterTheStack(t *testing.T) {
	root := tempDir(t)
	writeDefinition(t, root, `
compose: {files: [compose.yaml]}
ports: [postgres]
suites:
  unit: {service: app, command: [true]}
`)
	t.Chdir(root)
	h := newHarness(t)

	for _, args := range [][]string{{"ps"}, {"logs"}, {"logs", "app"}, {"exec", "web", "ls"}, {"down"}, {"down", "--volumes"}, {"compose", "down", "-v"}, {"test"}} {
		if _, err := h.run(t, args...); err != nil {
			t.Fatalf("aviary %s: %v", strings.Join(args, " "), err)
		}
		if _, err := os.Stat(h.reg.Path()); !os.IsNotExist(err) {
			t.Fatalf("aviary %s wrote the registry (stat err %v)", strings.Join(args, " "), err)
		}
	}
	if len(h.composed) != 7 {
		t.Fatalf("compose calls = %v, want ps, logs twice, exec and three downs", h.composed)
	}
	if port, ok := h.composed[0].stack.Env["AVIARY_PORT_POSTGRES"]; ok {
		t.Errorf("ps on an unregistered stack set AVIARY_PORT_POSTGRES=%s, want it unset", port)
	}

	if _, err := h.run(t, "up"); err != nil {
		t.Fatal(err)
	}
	if got := h.projectNames(t); len(got) != 1 {
		t.Fatalf("registry after up = %v, want the stack registered", got)
	}
	if _, err := h.run(t, "ps"); err != nil {
		t.Fatal(err)
	}
	if got, want := h.composed[len(h.composed)-1].stack.Env["AVIARY_PORT_POSTGRES"], basePort(root); got != want {
		t.Errorf("ps on a registered stack set AVIARY_PORT_POSTGRES=%q, want the registered %s", got, want)
	}
}

func TestEnvExportAndJSON(t *testing.T) {
	// The root holds characters a shell would expand or split on, so the
	// export lines are only right if eval hands the root back unchanged.
	root := filepath.Join(tempDir(t), "it's a $dir (copy)")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDefinition(t, root, "compose: {files: [compose.yaml]}\nports: [postgres]\n")
	t.Chdir(root)
	h := newHarness(t)
	port := basePort(root)

	out, err := h.run(t, "env", "--export")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 6 || !slices.Contains(lines, "export AVIARY_PORT_POSTGRES='"+port+"'") {
		t.Errorf("env --export =\n%s", out)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "export ") {
			t.Errorf("env --export line %q lacks export", line)
		}
	}
	evaled, err := exec.Command("sh", "-c", `eval "$1"; printf '%s\n' "$AVIARY_ROOT" "$AVIARY_PORT_POSTGRES"`, "sh", out).Output()
	if err != nil {
		t.Fatalf("eval of env --export: %v", err)
	}
	if want := root + "\n" + port + "\n"; string(evaled) != want {
		t.Errorf("after eval of env --export, AVIARY_ROOT and AVIARY_PORT_POSTGRES =\n%q, want\n%q", evaled, want)
	}

	out, err = h.run(t, "env")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(strings.Split(out, "\n"), "AVIARY_ROOT="+root) {
		t.Errorf("env without --export should print bare KEY=VALUE lines:\n%s", out)
	}

	out, err = h.run(t, "env", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("env --json is not a JSON object of strings: %v\n%s", err, out)
	}
	keys := slices.Sorted(maps.Keys(env))
	if want := []string{"AVIARY_CONFIG_DIR", "AVIARY_PORT_POSTGRES", "AVIARY_PROJECT", "AVIARY_ROOT", "AVIARY_SLUG", "COMPOSE_PROJECT_NAME"}; !slices.Equal(keys, want) {
		t.Errorf("env --json keys = %v, want %v", keys, want)
	}
	if env["AVIARY_PORT_POSTGRES"] != port {
		t.Errorf("AVIARY_PORT_POSTGRES = %q, want %s", env["AVIARY_PORT_POSTGRES"], port)
	}

	if _, err := h.run(t, "env", "--export", "--json"); err == nil {
		t.Error("env accepted --export with --json")
	}
}

// externalDefinitions makes a git repo with no aviary.yml as the working
// directory and two external definitions that differ in their port name.
func externalDefinitions(t *testing.T) (fromEnv, fromFlag string) {
	t.Helper()
	repo := tempDir(t)
	gitInit(t, repo)
	t.Chdir(repo)
	fromEnv, fromFlag = tempDir(t), tempDir(t)
	writeDefinition(t, fromEnv, "compose: {files: [compose.yaml]}\nports: [from_env]\n")
	writeDefinition(t, fromFlag, "compose: {files: [compose.yaml]}\nports: [from_flag]\n")
	return fromEnv, fromFlag
}

func envJSON(t *testing.T, h *harness, args ...string) map[string]string {
	t.Helper()
	out, err := h.run(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestConfigFlagWinsOverAviaryConfig(t *testing.T) {
	fromEnv, fromFlag := externalDefinitions(t)
	t.Setenv("AVIARY_CONFIG", fromEnv)

	if env := envJSON(t, newHarness(t), "env", "--json"); env["AVIARY_PORT_FROM_ENV"] == "" {
		t.Errorf("without --config, env = %v, want the AVIARY_CONFIG definition's port", env)
	}
	env := envJSON(t, newHarness(t), "--config", fromFlag, "env", "--json")
	if env["AVIARY_PORT_FROM_FLAG"] == "" || env["AVIARY_PORT_FROM_ENV"] != "" {
		t.Errorf("with --config, env = %v, want only the --config definition's port", env)
	}
}

func TestConfigDirIsTheDefinitionDirectory(t *testing.T) {
	t.Run("in-tree", func(t *testing.T) {
		root := tempDir(t)
		writeDefinition(t, root, "compose: {files: [compose.yaml]}\n")
		t.Chdir(root)
		h := newHarness(t)
		if env := envJSON(t, h, "env", "--json"); env["AVIARY_CONFIG_DIR"] != root {
			t.Errorf("env AVIARY_CONFIG_DIR = %q, want the worktree root %s", env["AVIARY_CONFIG_DIR"], root)
		}
		if _, err := h.run(t, "up"); err != nil {
			t.Fatal(err)
		}
		if got := h.composed[0].stack.Env["AVIARY_CONFIG_DIR"]; got != root {
			t.Errorf("compose env AVIARY_CONFIG_DIR = %q, want the worktree root %s", got, root)
		}
	})
	t.Run("external", func(t *testing.T) {
		_, fromFlag := externalDefinitions(t)
		h := newHarness(t)
		if env := envJSON(t, h, "--config", fromFlag, "env", "--json"); env["AVIARY_CONFIG_DIR"] != fromFlag || env["AVIARY_ROOT"] == fromFlag {
			t.Errorf("env = %v, want AVIARY_CONFIG_DIR %s apart from AVIARY_ROOT", env, fromFlag)
		}
		if _, err := h.run(t, "--config", fromFlag, "up"); err != nil {
			t.Fatal(err)
		}
		if got := h.composed[0].stack.Env["AVIARY_CONFIG_DIR"]; got != fromFlag {
			t.Errorf("compose env AVIARY_CONFIG_DIR = %q, want the definition directory %s", got, fromFlag)
		}
	})
}

func TestPassthroughTakesConfigBeforeTheCommandName(t *testing.T) {
	_, fromFlag := externalDefinitions(t)
	h := newHarness(t)

	if _, err := h.run(t, "--config", fromFlag, "up", "--build", "web"); err != nil {
		t.Fatal(err)
	}
	if len(h.composed) != 1 {
		t.Fatalf("compose calls = %v, want one", h.composed)
	}
	call := h.composed[0]
	if call.stack.Definition != fromFlag {
		t.Errorf("compose definition = %s, want %s", call.stack.Definition, fromFlag)
	}
	if want := []string{"up", "--detach", "--wait", "--build", "web"}; !slices.Equal(call.args, want) {
		t.Errorf("compose args = %q, want %q", call.args, want)
	}
}

func TestRequireEnvOnlyBlocksCommandsThatRunContainers(t *testing.T) {
	root := tempDir(t)
	writeDefinition(t, root, `
compose: {files: [compose.yaml]}
require_env: [AVIARY_TEST_MISSING]
suites:
  unit: {service: app, command: [true]}
`)
	t.Setenv("AVIARY_TEST_MISSING", "")
	t.Chdir(root)

	for _, args := range [][]string{{"ps"}, {"logs"}, {"logs", "app"}, {"exec", "web", "ls"}, {"env"}, {"test"}, {"compose", "config"}, {"compose", "-f", "x.yaml", "down", "-v"}} {
		if _, err := newHarness(t).run(t, args...); err != nil {
			t.Errorf("aviary %s failed on a missing require_env name: %v", strings.Join(args, " "), err)
		}
	}
	for _, args := range [][]string{{"up"}, {"run", "app"}, {"build"}, {"compose", "run", "app"}, {"compose", "--profile", "x", "up"}, {"init"}, {"test", "unit"}} {
		h := newHarness(t)
		_, err := h.run(t, args...)
		if err == nil || !strings.Contains(err.Error(), "AVIARY_TEST_MISSING") {
			t.Errorf("aviary %s: error = %v, want one naming AVIARY_TEST_MISSING", strings.Join(args, " "), err)
		}
		if len(h.composed) != 0 {
			t.Errorf("aviary %s ran compose despite a missing require_env name", strings.Join(args, " "))
		}
	}
}

func TestGCRemovesTheRestWhenOneStackFails(t *testing.T) {
	h, _ := lsFixture(t)
	h.register(t, "aviary-also-gone-444444", filepath.Join(filepath.Dir(h.goneRoot), "also-gone"), "")
	// Sorted first, so a gc that stopped at the first failure would skip
	// aviary-gone-222222.
	h.app.downProject = func(name string) error {
		h.downed = append(h.downed, name)
		if name == "aviary-also-gone-444444" {
			return &compose.ExitError{Code: 1}
		}
		return nil
	}

	_, err := h.run(t, "gc")
	if err == nil || !strings.Contains(err.Error(), "aviary-also-gone-444444") {
		t.Fatalf("gc error = %v, want one naming the stack that failed", err)
	}
	if want := []string{"aviary-also-gone-444444", "aviary-gone-222222"}; !slices.Equal(h.downed, want) {
		t.Errorf("gc took down %v, want %v", h.downed, want)
	}
	if got, want := h.projectNames(t), []string{"aviary-also-gone-444444", "aviary-live-111111"}; !slices.Equal(got, want) {
		t.Errorf("registry after gc = %v, want %v", got, want)
	}
}

func TestGCSkipsAWorktreeWhoseParentIsGoneToo(t *testing.T) {
	h := newHarness(t)
	h.register(t, "aviary-unmounted-555555", "/nonexistent/aviary-test/unmounted", "")

	out, err := h.run(t, "gc")
	if err != nil {
		t.Fatal(err)
	}
	if out != "" || len(h.downed) != 0 {
		t.Errorf("gc printed %q and took down %v, want nothing removed", out, h.downed)
	}
	if !strings.Contains(h.stderr.String(), "skipped") {
		t.Errorf("gc stderr = %q, want it to say the stack was skipped", h.stderr.String())
	}
	if got := h.projectNames(t); len(got) != 1 {
		t.Errorf("registry after gc = %v, want the stack kept", got)
	}
}

func TestComposeDownVolumesMarksTheStackUninitialized(t *testing.T) {
	root := tempDir(t)
	writeDefinition(t, root, "compose: {files: [compose.yaml]}\n")
	t.Chdir(root)
	h := newHarness(t)
	if _, err := h.run(t, "up"); err != nil {
		t.Fatal(err)
	}
	project := h.projectNames(t)[0]
	if _, err := h.reg.SetInitialized(project, true); err != nil {
		t.Fatal(err)
	}

	if _, err := h.run(t, "compose", "down"); err != nil {
		t.Fatal(err)
	}
	if h.entry(t, project).InitializedAt == nil {
		t.Error("compose down without volumes cleared initialized")
	}
	if _, err := h.run(t, "compose", "--profile", "x", "down", "--volumes"); err != nil {
		t.Fatal(err)
	}
	if h.entry(t, project).InitializedAt != nil {
		t.Error("compose down --volumes left the stack marked initialized")
	}
}

func TestComposeSubcommandSkipsGlobalFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		sub  string
		rest []string
	}{
		{[]string{"down", "-v"}, "down", []string{"-v"}},
		{[]string{"--profile", "x", "-f", "a.yaml", "down"}, "down", []string{}},
		{[]string{"--profile=x", "--dry-run", "run", "app", "true"}, "run", []string{"app", "true"}},
		{[]string{"--profile"}, "", nil},
	} {
		sub, rest := composeSubcommand(tc.args)
		if sub != tc.sub || !slices.Equal(rest, tc.rest) {
			t.Errorf("composeSubcommand(%q) = %q, %q, want %q, %q", tc.args, sub, rest, tc.sub, tc.rest)
		}
	}
}
