package compose

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestArgsPinsProjectDirectoryAndFiles(t *testing.T) {
	s := Stack{
		Project:    "aviary-wt-abc123",
		Root:       "/work/wt",
		Definition: "/defs/myrepo",
		Files:      []string{"compose.yaml", "/abs/compose.dev.yaml"},
	}
	got := s.Args("up", "--detach", "--wait", "postgres")
	want := []string{
		"compose",
		"--project-name", "aviary-wt-abc123",
		"--project-directory", "/work/wt",
		"--file", "/defs/myrepo/compose.yaml",
		"--file", "/abs/compose.dev.yaml",
		"up", "--detach", "--wait", "postgres",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Args:\n got %q\nwant %q", got, want)
	}
}

func TestArgsResolvesFilesAgainstDefinitionNotRoot(t *testing.T) {
	s := Stack{Project: "p", Root: "/work/wt", Definition: "/defs/myrepo", Files: []string{"sub/../compose.yaml"}}
	got := s.Args("ps")
	if !slices.Contains(got, "/defs/myrepo/compose.yaml") {
		t.Fatalf("Args %q: want file resolved under the definition dir", got)
	}
	if slices.Contains(got, "/work/wt/compose.yaml") {
		t.Fatalf("Args %q: file resolved under the root", got)
	}
}

func TestArgsPassesEachProfileBeforeFiles(t *testing.T) {
	s := Stack{
		Project:    "p",
		Root:       "/r",
		Definition: "/r",
		Files:      []string{"compose.yaml"},
		Profiles:   []string{"acceptance", "debug"},
	}
	got := s.Args("run", "--rm", "acceptance")
	want := []string{
		"compose",
		"--project-name", "p",
		"--project-directory", "/r",
		"--profile", "acceptance",
		"--profile", "debug",
		"--file", "/r/compose.yaml",
		"run", "--rm", "acceptance",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Args:\n got %q\nwant %q", got, want)
	}
}

func TestCommandSetsDirAndMergesEnv(t *testing.T) {
	t.Setenv("AVIARY_TEST_INHERITED", "host")
	t.Setenv("AVIARY_PORT_POSTGRES", "5432")
	s := Stack{
		Project:    "p",
		Root:       "/work/wt",
		Definition: "/defs",
		Files:      []string{"compose.yaml"},
		Env:        map[string]string{"AVIARY_PORT_POSTGRES": "20000", "AVIARY_PROJECT": "p"},
	}
	cmd := s.command("ps")
	if cmd.Dir != "/work/wt" {
		t.Fatalf("Dir = %q, want the root", cmd.Dir)
	}
	if want := append([]string{"docker"}, s.Args("ps")...); !slices.Equal(cmd.Args, want) {
		t.Fatalf("Args = %q, want %q", cmd.Args, want)
	}
	if !slices.Contains(cmd.Env, "AVIARY_TEST_INHERITED=host") {
		t.Fatal("inherited environment missing")
	}
	if got := lookup(cmd.Env, "AVIARY_PROJECT"); got != "p" {
		t.Fatalf("AVIARY_PROJECT = %q, want p", got)
	}
	// exec uses the last value for a repeated key, so the stack's port must
	// come after the inherited one.
	if got := lookup(cmd.Env, "AVIARY_PORT_POSTGRES"); got != "20000" {
		t.Fatalf("AVIARY_PORT_POSTGRES = %q, want the stack's 20000 to win", got)
	}
}

func TestEnvironIsDeterministic(t *testing.T) {
	s := Stack{Env: map[string]string{"B": "2", "A": "1", "C": "3"}}
	first := s.environ()
	for range 20 {
		if got := s.environ(); !slices.Equal(got, first) {
			t.Fatalf("environ order changed: %q vs %q", got, first)
		}
	}
	tail := first[len(first)-3:]
	if want := []string{"A=1", "B=2", "C=3"}; !slices.Equal(tail, want) {
		t.Fatalf("stack env tail = %q, want %q", tail, want)
	}
}

// lookup returns the value exec would pass for key: the last occurrence.
func lookup(env []string, key string) string {
	val := ""
	for _, kv := range env {
		if len(kv) > len(key) && kv[:len(key)] == key && kv[len(key)] == '=' {
			val = kv[len(key)+1:]
		}
	}
	return val
}

func TestExitErrorCarriesCode(t *testing.T) {
	err := exitError(exec.Command("sh", "-c", "exit 7").Run())
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v (%T), want *ExitError", err, err)
	}
	if ee.Code != 7 {
		t.Fatalf("Code = %d, want 7", ee.Code)
	}
	if ee.Error() != "docker compose exited with status 7" {
		t.Fatalf("Error() = %q", ee.Error())
	}
}

func TestExitErrorPassesOtherErrorsThrough(t *testing.T) {
	if err := exitError(nil); err != nil {
		t.Fatalf("exitError(nil) = %v", err)
	}
	err := exitError(exec.Command("/nonexistent/aviary-no-such-binary").Run())
	var ee *ExitError
	if err == nil || errors.As(err, &ee) {
		t.Fatalf("err = %v, want a start failure that is not an *ExitError", err)
	}
}

func TestExitErrorNamesTheSignalInsteadOfAStatus(t *testing.T) {
	err := exitError(exec.Command("sh", "-c", "kill -TERM $$").Run())
	var ee *ExitError
	if err == nil || errors.As(err, &ee) {
		t.Fatalf("err = %v (%T), want a signal error that is not an *ExitError", err, err)
	}
	if want := "docker compose terminated by signal terminated"; err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestParseResolved(t *testing.T) {
	data, err := os.ReadFile("testdata/config.json")
	if err != nil {
		t.Fatal(err)
	}
	r, err := parseResolved(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Volumes["postgres"].Name; got != "aviary-wt-abc123_postgres" {
		t.Fatalf("postgres volume name = %q", got)
	}
	if got := r.Services["redis"].Image; got != "redis:7" {
		t.Fatalf("redis image = %q", got)
	}
	// Two services mount postgres; the bind mount and the other volume do
	// not count, and the result is sorted regardless of map order.
	want := []string{"api:latest", "postgis/postgis:17-3.5"}
	if got := r.ImagesMounting("postgres"); !slices.Equal(got, want) {
		t.Fatalf("ImagesMounting(postgres) = %q, want %q", got, want)
	}
	if got := r.ImagesMounting("redis-data"); !slices.Equal(got, []string{"redis:7"}) {
		t.Fatalf("ImagesMounting(redis-data) = %q", got)
	}
	if got := r.ImagesMounting("missing"); len(got) != 0 {
		t.Fatalf("ImagesMounting(missing) = %q, want none", got)
	}
}

func TestParseResolvedRejectsBadJSON(t *testing.T) {
	if _, err := parseResolved([]byte("not json")); err == nil {
		t.Fatal("want an error for malformed compose config output")
	}
}

func TestParseProjects(t *testing.T) {
	out := []byte(`[{"Name":"aviary-wt-abc123","Status":"running(3)","ConfigFiles":"/defs/compose.yaml"},{"Name":"other","Status":"exited(1)","ConfigFiles":""}]`)
	got, err := parseProjects(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []Project{
		{Name: "aviary-wt-abc123", Status: "running(3)", ConfigFiles: "/defs/compose.yaml"},
		{Name: "other", Status: "exited(1)"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parseProjects = %+v, want %+v", got, want)
	}
	if _, err := parseProjects([]byte("{")); err == nil {
		t.Fatal("want an error for malformed compose ls output")
	}
}

func TestDownProjectArgsUseNameOnly(t *testing.T) {
	want := []string{"compose", "--project-name", "aviary-gone-ffffff", "down", "--volumes", "--remove-orphans"}
	if got := downProjectArgs("aviary-gone-ffffff"); !slices.Equal(got, want) {
		t.Fatalf("downProjectArgs = %q, want %q", got, want)
	}
}

func TestValidProjectName(t *testing.T) {
	for _, name := range []string{"aviary-wt-abc123", "p", "0a_b-c"} {
		if err := ValidProjectName(name); err != nil {
			t.Errorf("ValidProjectName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", "Bad Name", "-lead", "_lead", "UPPER", "a/b", "a.b"} {
		if err := ValidProjectName(name); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", name)) {
			t.Errorf("ValidProjectName(%q) = %v, want an error quoting the name", name, err)
		}
	}
}

// fakeDocker puts a `docker` on PATH that only records that it ran, so a
// test can tell whether compose would have been invoked.
func fakeDocker(t *testing.T) (marker string) {
	t.Helper()
	bin := t.TempDir()
	marker = filepath.Join(t.TempDir(), "ran")
	script := "#!/bin/sh\n: > '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return marker
}

func ran(t *testing.T, marker string) bool {
	t.Helper()
	_, err := os.Stat(marker)
	return err == nil
}

func TestDownProjectRefusesAnInvalidNameBeforeRunningCompose(t *testing.T) {
	for _, name := range []string{"", "Bad Name"} {
		t.Run(name, func(t *testing.T) {
			marker := fakeDocker(t)
			if err := DownProject(name); err == nil {
				t.Fatalf("DownProject(%q): want an error", name)
			}
			if ran(t, marker) {
				t.Fatalf("DownProject(%q) ran docker", name)
			}
		})
	}
	marker := fakeDocker(t)
	if err := DownProject("aviary-ok-abc123"); err != nil {
		t.Fatalf("DownProject of a valid name: %v", err)
	}
	if !ran(t, marker) {
		t.Fatal("DownProject of a valid name did not run docker")
	}
}

func TestStackRefusesAnInvalidProjectBeforeRunningCompose(t *testing.T) {
	marker := fakeDocker(t)
	s := Stack{Project: "", Root: t.TempDir(), Definition: "/defs", Files: []string{"compose.yaml"}}
	if err := s.Run("down", "--volumes"); err == nil {
		t.Fatal("Run with an empty project: want an error")
	}
	if _, err := s.Output("ps"); err == nil {
		t.Fatal("Output with an empty project: want an error")
	}
	if ran(t, marker) {
		t.Fatal("a stack with an empty project ran docker")
	}
}

// The child interrupts the test process, as a terminal's Ctrl-C interrupts
// both aviary and compose. runSignals must keep the process alive and return
// the child's own status once it exits.
func TestRunSignalsWaitsOutAnInterruptAndKeepsTheChildsStatus(t *testing.T) {
	cmd := exec.Command("sh", "-c", "kill -INT $PPID; sleep 0.2; exit 3")
	err := exitError(runSignals(cmd))
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 3 {
		t.Fatalf("runSignals = %v, want the child's status 3", err)
	}
}

func TestRunSignalsForwardsSIGTERM(t *testing.T) {
	cmd := exec.Command("sh", "-c", `trap 'exit 9' TERM; kill -TERM $PPID; sleep 5 & wait`)
	err := exitError(runSignals(cmd))
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 9 {
		t.Fatalf("runSignals = %v, want the child to get SIGTERM and exit 9", err)
	}
}
