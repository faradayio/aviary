package main

import (
	"bytes"
	"encoding/json"
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
	downed   []string
	downErr  error
	composed []composeCall
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
		listProjects: func() ([]compose.Project, error) { return h.projects, nil },
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

// run executes the CLI with args and returns what it printed to stdout.
func (h *harness) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := h.app.rootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
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

func TestUnknownCommandFails(t *testing.T) {
	_, err := newHarness(t).run(t, "bogus")
	if err == nil || !strings.Contains(err.Error(), `unknown command "bogus"`) {
		t.Errorf("aviary bogus: error = %v, want unknown command", err)
	}
}

// lsFixture registers an initialized in-tree stack whose worktree exists and
// is running, and an external one whose worktree is gone and that compose
// does not know about.
func lsFixture(t *testing.T) (h *harness, liveRoot string) {
	t.Helper()
	h = newHarness(t)
	liveRoot = tempDir(t)
	h.register(t, "aviary-live-111111", liveRoot, "", "postgres", "redis")
	if err := h.reg.SetInitialized("aviary-live-111111", true); err != nil {
		t.Fatal(err)
	}
	h.register(t, "aviary-gone-222222", "/nonexistent/aviary-test/gone", "/defs/myrepo")
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
	want := []map[string]any{
		{
			"project": "aviary-gone-222222", "status": "down", "root": "/nonexistent/aviary-test/gone",
			"branch": "", "worktree_missing": true, "initialized": false,
			"ports": map[string]any{}, "config": "/defs/myrepo",
		},
		{
			"project": "aviary-live-111111", "status": "running(2)", "root": liveRoot,
			"branch": "branch-of-" + filepath.Base(liveRoot), "worktree_missing": false, "initialized": true,
			"ports":  map[string]any{"postgres": float64(registry.FirstPort), "redis": float64(registry.FirstPort + 1)},
			"config": "in-tree",
		},
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
	for i, want := range [][]string{
		{"PROJECT", "STATUS", "BRANCH", "WORKTREE", "CONFIG", "PORTS"},
		{"aviary-gone-222222", "down", "(worktree", "missing)", "-", "/nonexistent/aviary-test/gone", "/defs/myrepo", "-"},
		{"aviary-live-111111", "running(2)", "branch-of-" + filepath.Base(liveRoot), liveRoot, "in-tree", "postgres=20000", "redis=20001"},
	} {
		if got := strings.Fields(lines[i]); !slices.Equal(got, want) {
			t.Errorf("line %d = %q, want fields %q", i, lines[i], want)
		}
	}
}

func TestGCDryRunReportsWithoutRemoving(t *testing.T) {
	h, _ := lsFixture(t)

	out, err := h.run(t, "gc", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if want := "aviary-gone-222222: worktree /nonexistent/aviary-test/gone is gone; would remove\n"; out != want {
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

func TestEnvExportAndJSON(t *testing.T) {
	root := tempDir(t)
	writeDefinition(t, root, "compose: {files: [compose.yaml]}\nports: [postgres]\n")
	t.Chdir(root)
	h := newHarness(t)

	out, err := h.run(t, "env", "--export")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 5 || !slices.Contains(lines, "export AVIARY_PORT_POSTGRES=20000") || !slices.Contains(lines, "export AVIARY_ROOT="+root) {
		t.Errorf("env --export =\n%s", out)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "export ") {
			t.Errorf("env --export line %q lacks export", line)
		}
	}

	out, err = h.run(t, "env", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("env --json is not a JSON object of strings: %v\n%s", err, out)
	}
	var keys []string
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"AVIARY_PORT_POSTGRES", "AVIARY_PROJECT", "AVIARY_ROOT", "AVIARY_SLUG", "COMPOSE_PROJECT_NAME"}; !slices.Equal(keys, want) {
		t.Errorf("env --json keys = %v, want %v", keys, want)
	}
	if env["AVIARY_PORT_POSTGRES"] != "20000" {
		t.Errorf("AVIARY_PORT_POSTGRES = %q", env["AVIARY_PORT_POSTGRES"])
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

	for _, args := range [][]string{{"ps"}, {"logs", "app"}, {"env"}, {"test"}} {
		if _, err := newHarness(t).run(t, args...); err != nil {
			t.Errorf("aviary %s failed on a missing require_env name: %v", strings.Join(args, " "), err)
		}
	}
	for _, args := range [][]string{{"up"}, {"run", "app"}, {"exec", "app", "sh"}, {"build"}, {"compose", "config"}, {"init"}, {"test", "unit"}} {
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
