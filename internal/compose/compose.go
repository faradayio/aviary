// Package compose invokes `docker compose` for one stack, pinning the
// project name, project directory, profiles and compose files so every
// subcommand addresses the same containers.
package compose

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
)

type Stack struct {
	Project string
	// Root is compose's project directory: bind mounts and env_file paths
	// in the compose files resolve against it, and compose runs from it.
	Root string
	// Definition is the directory holding aviary.yml. Relative Files
	// resolve against it, which differs from Root when the definition is
	// external to the worktree.
	Definition string
	Files      []string
	// Profiles are passed as --profile on every call.
	Profiles []string
	// Env is added to the inherited environment for compose interpolation,
	// taking precedence over inherited variables of the same name.
	Env map[string]string
}

// ExitError reports that compose itself ran but returned a non-zero status,
// so the caller can exit with the same code instead of wrapping it.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("docker compose exited with status %d", e.Code)
}

// Args returns the argv after `docker` for a compose subcommand:
// `compose --project-name P --project-directory Root [--profile p]...
// --file <file>... sub...`.
func (s Stack) Args(sub ...string) []string {
	argv := []string{"compose", "--project-name", s.Project, "--project-directory", s.Root}
	for _, p := range s.Profiles {
		argv = append(argv, "--profile", p)
	}
	for _, f := range s.Files {
		if !filepath.IsAbs(f) {
			f = filepath.Join(s.Definition, f)
		}
		argv = append(argv, "--file", f)
	}
	return append(argv, sub...)
}

// environ is the inherited environment followed by Env in key order. exec
// keeps the last value of a repeated key, so Env wins over the host.
func (s Stack) environ() []string {
	env := os.Environ()
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		env = append(env, k+"="+s.Env[k])
	}
	return env
}

func (s Stack) command(sub ...string) *exec.Cmd {
	cmd := exec.Command("docker", s.Args(sub...)...)
	cmd.Dir = s.Root
	cmd.Env = s.environ()
	return cmd
}

// Run executes a compose subcommand with the terminal attached.
func (s Stack) Run(args ...string) error {
	cmd := s.command(args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return exitError(cmd.Run())
}

// Output executes a compose subcommand and returns its stdout. Stderr still
// goes to the terminal so compose's own diagnostics are not lost.
func (s Stack) Output(args ...string) ([]byte, error) {
	cmd := s.command(args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, exitError(err)
	}
	return stdout.Bytes(), nil
}

func exitError(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &ExitError{Code: ee.ExitCode()}
	}
	return err
}

// Project is one row of `docker compose ls`.
type Project struct {
	Name        string `json:"Name"`
	Status      string `json:"Status"`
	ConfigFiles string `json:"ConfigFiles"`
}

// ListProjects returns every compose project the Docker daemon knows about,
// running or not.
func ListProjects() ([]Project, error) {
	cmd := exec.Command("docker", "compose", "ls", "--all", "--format", "json")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, exitError(err)
	}
	return parseProjects(out)
}

func parseProjects(out []byte) ([]Project, error) {
	var projects []Project
	if err := json.Unmarshal(out, &projects); err != nil {
		return nil, fmt.Errorf("parsing docker compose ls: %w", err)
	}
	return projects, nil
}

// DownProject removes a project's containers, networks and volumes using
// only its name. Compose can do this from container labels alone, which is
// what makes cleanup possible after the worktree and its files are gone.
func DownProject(name string) error {
	cmd := exec.Command("docker", downProjectArgs(name)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return exitError(cmd.Run())
}

func downProjectArgs(name string) []string {
	return []string{"compose", "--project-name", name, "down", "--volumes", "--remove-orphans"}
}

// Resolved is the part of `docker compose config` aviary needs: which
// image each service runs, which named volumes it mounts, and the Docker
// name of each named volume.
type Resolved struct {
	Services map[string]ResolvedService `json:"services"`
	Volumes  map[string]ResolvedVolume  `json:"volumes"`
}

type ResolvedService struct {
	Image   string          `json:"image"`
	Volumes []ResolvedMount `json:"volumes"`
}

type ResolvedMount struct {
	Type string `json:"type"`
	// Source is the volume's key in the top-level volumes map for a named
	// volume, or a host path for a bind mount.
	Source string `json:"source"`
}

type ResolvedVolume struct {
	// Name is the volume's Docker name, prefixed with the project.
	Name string `json:"name"`
}

// Config resolves the stack's compose files the way compose itself will.
func (s Stack) Config() (*Resolved, error) {
	out, err := s.Output("config", "--format", "json")
	if err != nil {
		return nil, err
	}
	return parseResolved(out)
}

func parseResolved(out []byte) (*Resolved, error) {
	var r Resolved
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("parsing docker compose config: %w", err)
	}
	return &r, nil
}

// ImagesMounting returns the images of every service that mounts the named
// volume, sorted.
func (r *Resolved) ImagesMounting(volume string) []string {
	var images []string
	for _, svc := range r.Services {
		for _, m := range svc.Volumes {
			if m.Type == "volume" && m.Source == volume {
				images = append(images, svc.Image)
			}
		}
	}
	slices.Sort(images)
	return images
}
