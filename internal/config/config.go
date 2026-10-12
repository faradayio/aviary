// Package config reads aviary.yml, the description of a stack: which compose
// files make it up, which host ports each worktree needs, how a fresh stack is
// seeded, which test suites it offers, and which volumes are cached as
// snapshots.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

const FileName = "aviary.yml"

type Config struct {
	Compose Compose `yaml:"compose"`
	// Ports names the host ports a worktree's stack publishes. Each name
	// becomes an AVIARY_PORT_<NAME> variable for compose interpolation.
	Ports []string `yaml:"ports"`
	// RequireEnv names host environment variables that must be set before
	// aviary calls compose, so a missing credential fails with a clear
	// message instead of a service boot failure.
	RequireEnv []string `yaml:"require_env"`
	// Init seeds a fresh stack from scratch, once per set of volumes.
	Init []Step `yaml:"init"`
	// AfterRestore brings a volume restored from a snapshot up to the
	// worktree's schema, for example by running migrations.
	AfterRestore []Step `yaml:"after_restore"`
	// Suites are the named test runs `aviary test <name>` offers.
	Suites map[string]Suite `yaml:"suites"`
	// Snapshot, when set, caches the named volumes after init so that
	// another worktree with the same seed inputs restores them instead of
	// running init again.
	Snapshot *Snapshot `yaml:"snapshot"`
}

type Compose struct {
	// Files are compose files, relative to the directory holding aviary.yml.
	Files []string `yaml:"files"`
	// Profiles are compose profiles enabled for every compose call.
	Profiles []string `yaml:"profiles"`
}

// Step is a command run in a fresh container of one service.
type Step struct {
	Service string   `yaml:"service"`
	Command []string `yaml:"command"`
}

type Suite struct {
	// Service is the container the suite runs in.
	Service string   `yaml:"service"`
	Command []string `yaml:"command"`
	// Needs lists the services that must be up and healthy first. The
	// suite's own service is started by `compose run` and need not be listed.
	Needs []string `yaml:"needs"`
	// Profiles are compose profiles enabled, in addition to the stack-wide
	// ones, for this suite's calls; services that exist only for test runs
	// sit behind them.
	Profiles []string `yaml:"profiles"`
	// Init is false for suites that run against an empty database and
	// should not trigger the stack's init steps.
	Init *bool `yaml:"init"`
}

type Snapshot struct {
	// Volumes are compose volume names (the keys under the compose file's
	// top-level `volumes`).
	Volumes []string `yaml:"volumes"`
	// KeyFiles are paths, relative to the project root, holding the seed
	// inputs. A change to any of them means a cached snapshot cannot be
	// reused and init runs from scratch.
	KeyFiles []string `yaml:"key_files"`
	// StampFiles are paths, relative to the project root, describing the
	// schema. Snapshots are kept per stamp, and a restore from a different
	// stamp runs the after_restore steps.
	StampFiles []string `yaml:"stamp_files"`
}

// WantsInit reports whether the suite should run the init steps first.
func (s Suite) WantsInit() bool {
	return s.Init == nil || *s.Init
}

var (
	portName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Load reads aviary.yml from dir.
func Load(dir string) (*Config, error) {
	path := filepath.Join(dir, FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if len(c.Compose.Files) == 0 {
		return fmt.Errorf("compose.files must list at least one compose file")
	}
	if err := noEmpty("compose.profiles", c.Compose.Profiles); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, name := range c.Ports {
		if !portName.MatchString(name) {
			return fmt.Errorf("port name %q must match %s", name, portName)
		}
		if seen[name] {
			return fmt.Errorf("port name %q listed twice", name)
		}
		seen[name] = true
	}
	for _, name := range c.RequireEnv {
		if !envName.MatchString(name) {
			return fmt.Errorf("require_env name %q must match %s", name, envName)
		}
	}
	for i, step := range c.Init {
		if err := step.validate(); err != nil {
			return fmt.Errorf("init[%d]: %w", i, err)
		}
	}
	for i, step := range c.AfterRestore {
		if err := step.validate(); err != nil {
			return fmt.Errorf("after_restore[%d]: %w", i, err)
		}
	}
	names := make([]string, 0, len(c.Suites))
	for name := range c.Suites {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := c.Suites[name].validate(); err != nil {
			return fmt.Errorf("suite %q: %w", name, err)
		}
	}
	if c.Snapshot != nil && len(c.Snapshot.Volumes) == 0 {
		return fmt.Errorf("snapshot.volumes must name at least one volume")
	}
	return nil
}

func (s Step) validate() error {
	if s.Service == "" {
		return fmt.Errorf("needs a service")
	}
	if len(s.Command) == 0 {
		return fmt.Errorf("needs a command")
	}
	return nil
}

func (s Suite) validate() error {
	if err := (Step{Service: s.Service, Command: s.Command}).validate(); err != nil {
		return err
	}
	if err := noEmpty("needs", s.Needs); err != nil {
		return err
	}
	return noEmpty("profiles", s.Profiles)
}

func noEmpty(field string, values []string) error {
	for i, v := range values {
		if v == "" {
			return fmt.Errorf("%s[%d] is empty", field, i)
		}
	}
	return nil
}
