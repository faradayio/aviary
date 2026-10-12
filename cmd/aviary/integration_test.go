//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/faradayio/aviary/internal/compose"
	"github.com/faradayio/aviary/internal/registry"
)

// TestMinimalExampleEndToEnd runs a temp copy of examples/minimal through the
// real aviary binary and Docker: up, both suites, ls, down and rm. State and
// cache go to temp XDG directories, so the machine's real registry is never
// read or written. Run with: go test -tags integration ./cmd/aviary
func TestMinimalExampleEndToEnd(t *testing.T) {
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err != nil {
		t.Fatalf("the integration test needs docker compose: %v\n%s", err, out)
	}

	tmp := t.TempDir()
	bin := filepath.Join(tmp, "aviary")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	worktree := filepath.Join(tmp, "minimal")
	if err := os.CopyFS(worktree, os.DirFS(filepath.Join("..", "..", "examples", "minimal"))); err != nil {
		t.Fatal(err)
	}
	before := files(t, worktree)
	stateHome := filepath.Join(tmp, "state")
	env := append(withoutAviaryVars(os.Environ()),
		"XDG_STATE_HOME="+stateHome,
		"XDG_CACHE_HOME="+filepath.Join(tmp, "cache"),
	)
	aviary := func(t *testing.T, args ...string) (stdout string, code int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = worktree
		cmd.Env = env
		var out, errOut bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errOut
		err := cmd.Run()
		var exit *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exit):
			code = exit.ExitCode()
		default:
			t.Fatalf("aviary %s: %v", strings.Join(args, " "), err)
		}
		t.Logf("aviary %s (exit %d)\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, &out, &errOut)
		return out.String(), code
	}
	mustRun := func(t *testing.T, args ...string) string {
		t.Helper()
		out, code := aviary(t, args...)
		if code != 0 {
			t.Fatalf("aviary %s exited %d", strings.Join(args, " "), code)
		}
		return out
	}

	var envOut map[string]string
	if err := json.Unmarshal([]byte(mustRun(t, "env", "--json")), &envOut); err != nil {
		t.Fatal(err)
	}
	project := envOut["AVIARY_PROJECT"]
	if !strings.HasPrefix(project, "aviary-minimal-") {
		t.Fatalf("project = %q", project)
	}
	// Removes the stack by its compose labels even when the test fails
	// halfway; a no-op once rm has run.
	t.Cleanup(func() {
		if err := compose.DownProject(project); err != nil {
			t.Errorf("cleaning up %s: %v", project, err)
		}
	})

	mustRun(t, "up")

	// The published host port is the one the registry allocated.
	published := strings.TrimSpace(mustRun(t, "compose", "port", "postgres", "5432"))
	if !strings.HasSuffix(published, ":"+envOut["AVIARY_PORT_POSTGRES"]) {
		t.Errorf("postgres published on %q, want port %s", published, envOut["AVIARY_PORT_POSTGRES"])
	}

	// db wants init, so the first run seeds two rows and then counts them.
	if got := lastLine(mustRun(t, "test", "db")); got != "2" {
		t.Errorf("test db printed %q last, want the seeded count 2", got)
	}
	// Init ran once; a second run must not seed again.
	if got := lastLine(mustRun(t, "test", "db")); got != "2" {
		t.Errorf("second test db printed %q last, want 2", got)
	}

	// smoke skips init, takes its script after --, and sees the project name
	// through compose interpolation.
	if got := lastLine(mustRun(t, "test", "smoke", "--", `echo "$AVIARY_PROJECT"`)); got != project {
		t.Errorf("smoke saw AVIARY_PROJECT=%q, want %q", got, project)
	}
	if _, code := aviary(t, "test", "smoke", "--", "exit 3"); code != 3 {
		t.Errorf("a suite exiting 3 made aviary exit %d", code)
	}

	var rows []stackRow
	if err := json.Unmarshal([]byte(mustRun(t, "ls", "--json")), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("ls --json = %+v, want one stack", rows)
	}
	row := rows[0]
	if row.Project != project || !row.Initialized || row.Config != "in-tree" || row.WorktreeMissing {
		t.Errorf("ls row = %+v", row)
	}
	if !strings.HasPrefix(row.Status, "running") {
		t.Errorf("status = %q, want running", row.Status)
	}
	for _, name := range []string{"postgres", "redis"} {
		if row.Ports[name] < registry.FirstPort {
			t.Errorf("port %s = %d, want >= %d", name, row.Ports[name], registry.FirstPort)
		}
	}

	mustRun(t, "down", "--volumes")
	mustRun(t, "rm")

	entries, err := registry.OpenAt(filepath.Join(stateHome, "aviary", "registry.json")).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("registry after rm = %+v, want empty", entries)
	}
	if after := files(t, worktree); !slices.Equal(before, after) {
		t.Errorf("aviary changed the worktree: before %v, after %v", before, after)
	}
}

// withoutAviaryVars drops variables that would point the binary at a
// different definition or registry than the test's.
func withoutAviaryVars(environ []string) []string {
	return slices.DeleteFunc(slices.Clone(environ), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return strings.HasPrefix(name, "AVIARY_") || strings.HasPrefix(name, "COMPOSE_") ||
			name == "XDG_STATE_HOME" || name == "XDG_CACHE_HOME"
	})
}

// files lists every path under dir, relative to it.
func files(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		paths = append(paths, rel)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
