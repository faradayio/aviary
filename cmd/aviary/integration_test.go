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

	regPath := filepath.Join(stateHome, "aviary", "registry.json")
	registered := func(t *testing.T) []registry.Entry {
		t.Helper()
		entries, err := registry.OpenAt(regPath).List()
		if err != nil {
			t.Fatal(err)
		}
		return entries
	}

	// Looking at a never-started stack must not register it.
	mustRun(t, "ps")
	if entries := registered(t); len(entries) != 0 {
		t.Fatalf("registry after ps on a never-started stack = %+v, want empty", entries)
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

	if entries := registered(t); len(entries) != 0 {
		t.Errorf("registry after rm = %+v, want empty", entries)
	}
	if after := files(t, worktree); !slices.Equal(before, after) {
		t.Errorf("aviary changed the worktree: before %v, after %v", before, after)
	}
}

func TestSnapshotSaveRestoreAndAfterRestoreEndToEnd(t *testing.T) {
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err != nil {
		t.Fatalf("the integration test needs docker compose: %v\n%s", err, out)
	}

	tmp := t.TempDir()
	bin := filepath.Join(tmp, "aviary")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	env := append(withoutAviaryVars(os.Environ()),
		"XDG_STATE_HOME="+filepath.Join(tmp, "state"),
		"XDG_CACHE_HOME="+filepath.Join(tmp, "cache"),
	)

	const aviaryYml = `compose:
  files: [compose.yaml]
ports: [postgres, redis]
init:
  - service: client
    command: [psql, -f, /src/seed.sql]
after_restore:
  - service: client
    command: [psql, -f, /src/migrate.sql]
snapshot:
  volumes: [postgres]
  key_files: [seed.sql]
  stamp_files: [schema.sql]
suites:
  db:
    service: client
    needs: [postgres]
    command: [psql, -tAc, "SELECT count(*) FROM seeded"]
`

	makeWorktree := func(name, schema, migrate string) string {
		dir := filepath.Join(tmp, name)
		if err := os.CopyFS(dir, os.DirFS(filepath.Join("..", "..", "examples", "minimal"))); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "aviary.yml"), []byte(aviaryYml), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "seed.sql"), []byte("CREATE TABLE seeded (id int); INSERT INTO seeded VALUES (1), (2);\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "schema.sql"), []byte(schema), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "migrate.sql"), []byte(migrate), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	runIn := func(dir string, args ...string) (stdout, stderr string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = dir
		cmd.Env = env
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			t.Fatalf("aviary %s in %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), dir, err, &outBuf, &errBuf)
		}
		return outBuf.String(), errBuf.String()
	}

	wt1 := makeWorktree("snap1", "-- v1\n", "SELECT 1;\n")
	wt2 := makeWorktree("snap2", "-- v1\n", "SELECT 1;\n")
	wt3 := makeWorktree("snap3", "-- v2\n", "INSERT INTO seeded VALUES (3);\n")

	for _, dir := range []string{wt1, wt2, wt3} {
		out, _ := runIn(dir, "env", "--json")
		var e map[string]string
		if err := json.Unmarshal([]byte(out), &e); err != nil {
			t.Fatal(err)
		}
		proj := e["AVIARY_PROJECT"]
		t.Cleanup(func() { _ = compose.DownProject(proj) })
	}

	// First worktree seeds from scratch and saves the snapshot.
	out1, err1 := runIn(wt1, "test", "db")
	if got := lastLine(out1); got != "2" {
		t.Fatalf("wt1 test db = %q, want 2", got)
	}
	if !strings.Contains(err1, "saved snapshot") {
		t.Errorf("wt1 stderr missing saved snapshot:\n%s", err1)
	}

	// Second worktree on the same schema restores without running init steps.
	out2, err2 := runIn(wt2, "test", "db")
	if got := lastLine(out2); got != "2" {
		t.Fatalf("wt2 test db = %q, want 2", got)
	}
	if !strings.Contains(err2, "restoring snapshot") || strings.Contains(err2, "init: client psql") {
		t.Errorf("wt2 stderr should restore snapshot without running init:\n%s", err2)
	}

	// Third worktree on a newer schema restores and runs after_restore.
	out3, err3 := runIn(wt3, "test", "db")
	if got := lastLine(out3); got != "3" {
		t.Fatalf("wt3 test db = %q, want 3", got)
	}
	if !strings.Contains(err3, "after_restore") {
		t.Errorf("wt3 stderr should run after_restore:\n%s", err3)
	}

	for _, dir := range []string{wt1, wt2, wt3} {
		runIn(dir, "rm")
	}
}

func TestIdleWarmStackAutoStopsWhileKeepingVolume(t *testing.T) {
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err != nil {
		t.Fatalf("the integration test needs docker compose: %v\n%s", err, out)
	}

	tmp := t.TempDir()
	bin := filepath.Join(tmp, "aviary")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	worktree := filepath.Join(tmp, "warm")
	if err := os.CopyFS(worktree, os.DirFS(filepath.Join("..", "..", "examples", "minimal"))); err != nil {
		t.Fatal(err)
	}
	const aviaryYml = `compose:
  files: [compose.yaml]
idle_ttl: 2s
ports: [postgres, redis]
init:
  - service: client
    command: [psql, -c, "CREATE TABLE items (n int); INSERT INTO items VALUES (1), (2)"]
suites:
  db:
    service: client
    needs: [postgres]
    command: [psql, -tAc, "SELECT count(*) FROM items"]
`
	if err := os.WriteFile(filepath.Join(worktree, "aviary.yml"), []byte(aviaryYml), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(withoutAviaryVars(os.Environ()),
		"XDG_STATE_HOME="+filepath.Join(tmp, "state"),
		"XDG_CACHE_HOME="+filepath.Join(tmp, "cache"),
	)
	run := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = worktree
		cmd.Env = env
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			t.Fatalf("aviary %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, &outBuf, &errBuf)
		}
		return outBuf.String()
	}

	var envOut map[string]string
	if err := json.Unmarshal([]byte(run("env", "--json")), &envOut); err != nil {
		t.Fatal(err)
	}
	project := envOut["AVIARY_PROJECT"]
	t.Cleanup(func() { _ = compose.DownProject(project) })

	if got := lastLine(run("test", "db")); got != "2" {
		t.Fatalf("first test db = %q, want 2", got)
	}

	deadline := time.Now().Add(20 * time.Second)
	stopped := false
	for time.Now().Before(deadline) {
		var rows []stackRow
		if err := json.Unmarshal([]byte(run("ls", "--json")), &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].Status == "down" && rows[0].LastUsedAt == nil {
			if !rows[0].Initialized {
				t.Fatalf("idle stop cleared initialized: %+v", rows[0])
			}
			stopped = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !stopped {
		t.Fatalf("warm stack %s was not auto-stopped within 20s:\n%s", project, run("ls", "--json"))
	}

	if got := lastLine(run("test", "db")); got != "2" {
		t.Fatalf("second test db after idle stop = %q, want 2", got)
	}
	run("rm")
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
