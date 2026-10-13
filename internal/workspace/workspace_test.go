package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/faradayio/aviary/internal/config"
)

func TestSlugIsComposeSafeAndDistinctPerPath(t *testing.T) {
	a := Slug("/home/me/work/My Repo.git")
	b := Slug("/home/you/work/My Repo.git")
	if a == b {
		t.Fatalf("same basename in different directories produced the same slug %q", a)
	}
	safe := regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	for _, s := range []string{a, b, Slug("/"), Slug("/x/" + strings.Repeat("a", 80)), Slug("/x/" + string(make([]byte, 80)))} {
		if !safe.MatchString(s) {
			t.Errorf("slug %q is not compose-safe", s)
		}
	}
	if got := Slug("/tmp/wt-a"); !strings.HasPrefix(got, "wt-a-") {
		t.Errorf("slug should start with the basename, got %q", got)
	}
	if got := Slug("/x/" + strings.Repeat("a", 80)); len(got) != 30+1+6 {
		t.Errorf("long basename should be capped at 30 characters, got %q", got)
	}
}

// isolate keeps git from discovering a repository above dir, so tests behave
// the same wherever the temp directory lives.
func isolate(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
}

// tempDir returns a fresh temp directory with symlinks resolved, since git
// reports resolved paths (macOS temp dirs live behind /var -> /private/var).
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeDefinition(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte("compose:\n  files: [c.yaml]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, parts ...string) string {
	t.Helper()
	dir := filepath.Join(parts...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func gitInit(t *testing.T, dir, branch string) {
	t.Helper()
	isolate(t, dir)
	out, err := exec.Command("git", "-C", dir, "init", "--quiet", "-b", branch).CombinedOutput()
	if err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
}

func TestDetectWalksUpToAnInTreeDefinition(t *testing.T) {
	root := tempDir(t)
	writeDefinition(t, root)
	nested := mkdir(t, root, "src", "deep")

	ws, err := Detect(nested, "")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Root != root || ws.Definition != root {
		t.Errorf("Root = %q, Definition = %q, want both %q", ws.Root, ws.Definition, root)
	}
	if ws.External {
		t.Error("in-tree definition reported as external")
	}
	if ws.Slug != Slug(root) {
		t.Errorf("Slug = %q, want %q", ws.Slug, Slug(root))
	}
	if ws.Project() != ProjectPrefix+ws.Slug {
		t.Errorf("Project() = %q", ws.Project())
	}
}

func TestDetectPrefersAnInTreeDefinitionOverConfigDir(t *testing.T) {
	root := tempDir(t)
	writeDefinition(t, root)
	external := tempDir(t)
	writeDefinition(t, external)

	ws, err := Detect(root, external)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Definition != root || ws.External {
		t.Errorf("Definition = %q, External = %v; want in-tree %q", ws.Definition, ws.External, root)
	}
}

func TestDetectUsesExternalDefinitionWithGitToplevelAsRoot(t *testing.T) {
	repo := tempDir(t)
	gitInit(t, repo, "main")
	nested := mkdir(t, repo, "src", "deep")
	external := tempDir(t)
	writeDefinition(t, external)

	ws, err := Detect(nested, external)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Root != repo {
		t.Errorf("Root = %q, want git toplevel %q", ws.Root, repo)
	}
	if ws.Definition != external {
		t.Errorf("Definition = %q, want %q", ws.Definition, external)
	}
	if !ws.External {
		t.Error("External = false for a definition from configDir")
	}
	if ws.Slug != Slug(repo) {
		t.Errorf("Slug = %q, want slug of the root %q", ws.Slug, Slug(repo))
	}
}

func TestDetectExternalOutsideAGitRepoNamesBothLookups(t *testing.T) {
	cwd := tempDir(t)
	isolate(t, cwd)
	external := tempDir(t)
	writeDefinition(t, external)

	_, err := Detect(cwd, external)
	if err == nil {
		t.Fatal("expected an error when cwd is not in a git repository")
	}
	for _, want := range []string{config.FileName, cwd, external, "git"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestDetectConfigDirWithoutDefinitionNamesBothLookups(t *testing.T) {
	repo := tempDir(t)
	gitInit(t, repo, "main")
	external := tempDir(t)

	_, err := Detect(repo, external)
	if err == nil {
		t.Fatal("expected an error when configDir holds no aviary.yml")
	}
	for _, want := range []string{config.FileName, repo, external} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestDetectWithNeitherDefinitionNamesBothLookups(t *testing.T) {
	cwd := tempDir(t)

	_, err := Detect(cwd, "")
	if err == nil {
		t.Fatal("expected an error when no aviary.yml exists")
	}
	for _, want := range []string{config.FileName, cwd, "AVIARY_CONFIG", "--config"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestBranchReportsTheCurrentBranch(t *testing.T) {
	repo := tempDir(t)
	gitInit(t, repo, "feature-x")
	if got := Branch(repo); got != "feature-x" {
		t.Errorf("Branch = %q, want %q", got, "feature-x")
	}
}

func TestBranchOutsideARepoIsEmpty(t *testing.T) {
	dir := tempDir(t)
	isolate(t, dir)
	if got := Branch(dir); got != "" {
		t.Errorf("Branch = %q, want empty outside a repository", got)
	}
	if got := Branch(filepath.Join(dir, "missing")); got != "" {
		t.Errorf("Branch = %q, want empty for a missing directory", got)
	}
}

func TestDetectThroughASymlinkGivesTheSameRoot(t *testing.T) {
	root := tempDir(t)
	writeDefinition(t, root)
	link := filepath.Join(tempDir(t), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}

	ws, err := Detect(link, "")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Root != root || ws.Slug != Slug(root) {
		t.Errorf("Root = %q, Slug = %q; want the resolved root %q", ws.Root, ws.Slug, root)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	base := []string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}
	out, err := exec.Command("git", append(base, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// nestedWorktree makes a parent checkout holding aviary.yml and a worktree of
// it at .claude/worktrees/x whose branch has no aviary.yml, the layout Claude
// Code creates by default. It returns the parent and the nested worktree.
func nestedWorktree(t *testing.T) (string, string) {
	t.Helper()
	parent := tempDir(t)
	gitInit(t, parent, "main")
	git(t, parent, "commit", "--quiet", "--allow-empty", "-m", "initial")
	nested := filepath.Join(parent, ".claude", "worktrees", "x")
	git(t, parent, "worktree", "add", "--quiet", "-b", "x", nested)
	writeDefinition(t, parent)
	return parent, nested
}

func TestDetectInANestedWorktreeIgnoresTheParentDefinition(t *testing.T) {
	parent, nested := nestedWorktree(t)
	external := tempDir(t)
	writeDefinition(t, external)

	ws, err := Detect(nested, external)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Root != nested || ws.Definition != external || !ws.External {
		t.Errorf("Root = %q, Definition = %q, External = %v; want root %q with external %q", ws.Root, ws.Definition, ws.External, nested, external)
	}
	parentWs, err := Detect(parent, "")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Project() == parentWs.Project() {
		t.Errorf("nested worktree shares the parent's project %q", ws.Project())
	}
}

func TestDetectInANestedWorktreeWithoutConfigErrors(t *testing.T) {
	parent, nested := nestedWorktree(t)
	sub := mkdir(t, nested, "src")

	ws, err := Detect(sub, "")
	if err == nil {
		t.Fatalf("expected an error, got the definition at %q (parent %q)", ws.Definition, parent)
	}
	for _, want := range []string{config.FileName, sub, "up to the checkout top " + nested, "AVIARY_CONFIG"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestDetectFindsADefinitionAtOrBelowTheGitBoundary(t *testing.T) {
	repo := tempDir(t)
	gitInit(t, repo, "main")
	writeDefinition(t, repo)
	deep := mkdir(t, repo, "src", "deep")

	ws, err := Detect(deep, "")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Root != repo {
		t.Errorf("Root = %q, want the repository root %q holding aviary.yml", ws.Root, repo)
	}

	sub := mkdir(t, repo, "services")
	writeDefinition(t, sub)
	ws, err = Detect(sub, "")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Root != sub {
		t.Errorf("Root = %q, want the nearer definition %q", ws.Root, sub)
	}
}

func TestDetectTreatsAGitFileAsABoundary(t *testing.T) {
	parent := tempDir(t)
	writeDefinition(t, parent)
	worktree := mkdir(t, parent, "wt")
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /nowhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ws, err := Detect(mkdir(t, worktree, "src"), "")
	if err == nil {
		t.Fatalf("walk-up crossed a .git file and found %q", ws.Definition)
	}
}
