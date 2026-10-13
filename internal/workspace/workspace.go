// Package workspace locates the stack definition aviary runs from and the
// worktree root it runs against, and derives the compose project name that
// keeps that worktree's stack apart from every other checkout's.
package workspace

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/faradayio/aviary/internal/config"
)

// ProjectPrefix marks compose projects that aviary owns, so `aviary ls` can
// tell them apart from stacks started by hand.
const ProjectPrefix = "aviary-"

type Workspace struct {
	// Root is the worktree the stack runs against: compose's project
	// directory, against which bind mounts, env_file and snapshot key files
	// resolve. Each root gets its own stack.
	Root string
	// Definition is the directory holding aviary.yml; compose file paths in
	// aviary.yml are relative to it. Equal to Root for an in-tree definition.
	Definition string
	// Slug is a filesystem- and compose-safe name for Root.
	Slug string
	// External is true when the definition came from --config or
	// AVIARY_CONFIG rather than from aviary.yml inside the worktree.
	External bool
}

// Project is the compose project name for this workspace's stack.
func (w Workspace) Project() string {
	return ProjectPrefix + w.Slug
}

// Detect finds the stack definition for cwd. The nearest aviary.yml in cwd or
// a parent, up to the enclosing git checkout's top directory, wins, and its
// directory is both Root and Definition. Otherwise configDir (from --config or
// AVIARY_CONFIG, empty when neither is set) must hold aviary.yml, and Root is
// the git toplevel of cwd. Paths are absolute with symlinks resolved, so the
// same worktree always gets the same slug.
func Detect(cwd, configDir string) (Workspace, error) {
	cwd, err := canonical(cwd)
	if err != nil {
		return Workspace{}, err
	}
	dir, checkoutTop := findUp(cwd)
	if dir != "" {
		return Workspace{Root: dir, Definition: dir, Slug: Slug(dir)}, nil
	}

	inTree := fmt.Sprintf("no %s in %s or any parent", config.FileName, cwd)
	if checkoutTop != "" {
		inTree += " up to the checkout top " + checkoutTop
	}
	if configDir == "" {
		return Workspace{}, fmt.Errorf("%s, and no external definition is set (--config or AVIARY_CONFIG)", inTree)
	}
	definition, err := canonical(configDir)
	if err != nil {
		return Workspace{}, fmt.Errorf("%s, and the external definition %s cannot be read: %w", inTree, configDir, err)
	}
	if _, err := os.Stat(filepath.Join(definition, config.FileName)); err != nil {
		return Workspace{}, fmt.Errorf("%s, and the external definition %s has no %s: %w", inTree, definition, config.FileName, err)
	}
	root, err := gitToplevel(cwd)
	if err != nil {
		return Workspace{}, fmt.Errorf("%s, and the external definition %s needs a git repository to find the worktree root: %w", inTree, definition, err)
	}
	return Workspace{Root: root, Definition: definition, Slug: Slug(root), External: true}, nil
}

// Branch returns the branch checked out at root, or "" when root is not a git
// repository, HEAD is detached, or git is unavailable. It never fails, since
// the branch is only shown to people.
func Branch(root string) string {
	out, err := exec.Command("git", "-C", root, "symbolic-ref", "--quiet", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func canonical(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// findUp returns the nearest directory at or above dir that holds aviary.yml,
// searching no higher than the first directory holding a .git entry (a
// directory in a main checkout, a file in a linked worktree). Stopping there
// keeps a worktree nested inside another checkout from adopting that
// checkout's definition, and with it its root and stack. When nothing is
// found, definition is empty and checkoutTop is the directory holding .git
// where the search stopped, or empty when it reached the filesystem root.
func findUp(dir string) (definition, checkoutTop string) {
	for {
		if _, err := os.Stat(filepath.Join(dir, config.FileName)); err == nil {
			return dir, ""
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return "", dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

func gitToplevel(dir string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git rev-parse --show-toplevel in %s: %s", dir, msg)
		}
		return "", fmt.Errorf("git rev-parse --show-toplevel in %s: %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

var unsafe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug names a workspace by its directory basename plus a short hash of its
// full path. The basename keeps `docker ps` readable; the hash keeps two
// checkouts with the same basename from colliding.
func Slug(root string) string {
	base := strings.ToLower(filepath.Base(root))
	base = strings.Trim(unsafe.ReplaceAllString(base, "-"), "-")
	if len(base) > 30 {
		base = strings.Trim(base[:30], "-")
	}
	sum := sha1.Sum([]byte(root))
	hash := hex.EncodeToString(sum[:])[:6]
	if base == "" {
		return hash
	}
	return base + "-" + hash
}
