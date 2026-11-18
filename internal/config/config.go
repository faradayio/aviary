// Package config reads aviary.yml, the description of a stack: which compose
// files make it up, which host ports each worktree needs, how a fresh stack is
// seeded, which test suites it offers, and which volumes are cached as
// snapshots.
package config

import (
	"bytes"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	FileName       = "aviary.yml"
	DefaultIdleTTL = 20 * time.Minute
)

type Config struct {
	Compose Compose `yaml:"compose"`
	// IdleTTL is how long containers started transiently (`test`, `init`,
	// `run`) stay warm after the command exits before aviary stops them
	// while keeping their volumes (default 20m). Stacks started with `up`
	// stay running until `down`.
	IdleTTL string `yaml:"idle_ttl,omitempty"`
	// Ports names the host ports a worktree's stack publishes. Each name
	// becomes an AVIARY_PORT_<NAME> variable for compose interpolation.
	Ports []string `yaml:"ports"`
	// RequireEnv names host environment variables that must be set before
	// aviary calls compose, so a missing credential fails with a clear
	// message instead of a service boot failure.
	RequireEnv []string `yaml:"require_env"`
	// Vault, when set, mints and caches a child VAULT_TOKEN from the
	// developer's ~/.vault-token when VAULT_TOKEN is absent or expired.
	Vault *Vault `yaml:"vault"`
	// HostEnv fills unset host environment variables from standard host
	// files (such as Google Application Default Credentials) before
	// require_env and compose run.
	HostEnv map[string]HostFile `yaml:"host_env"`
	// Prepare lists host commands run in the worktree root before init and
	// before suites that do not opt out, such as copying gitignored API
	// artifacts into a fresh worktree.
	Prepare []HostStep `yaml:"prepare"`
	// Init seeds a fresh stack from scratch. It runs once after the stack is
	// created and again after aviary removes its volumes (`down --volumes`
	// or `rm`).
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

type Vault struct {
	// Addr is the Vault server URL, such as https://vault2.faraday.ai.
	Addr string `yaml:"addr"`
	// TTL is the Go duration requested for minted child tokens (default 8h).
	TTL string `yaml:"ttl"`
}

type HostFile struct {
	// Path is a host file path (`~/` expands to the user's home directory)
	// assigned to the environment variable when the variable is unset on
	// the host and the file exists.
	Path string `yaml:"path"`
	// Required fails commands that start containers when the variable is
	// unset and Path does not exist.
	Required bool `yaml:"required"`
	// Hint is appended to the error when Required is true and Path is
	// missing, telling the user or agent what login command creates it.
	Hint string `yaml:"hint"`
}

// HostStep is a command run on the host in the worktree root.
type HostStep struct {
	Command []string `yaml:"command"`
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
	// Prepare is false for suites that do not need the stack's host
	// prepare steps to run first.
	Prepare *bool `yaml:"prepare"`
	// Host is true when Command runs directly on the host in Workdir (relative
	// to the worktree root) instead of `docker compose run` inside Service,
	// with the stack's AVIARY_* environment variables and Suite.Env set.
	Host bool `yaml:"host"`
	// Workdir is the suite's package directory, relative to the worktree root.
	// When Host is true, Command runs in Workdir on the host. For every suite
	// with Workdir set, aviary strips the package directory prefix from path
	// arguments passed to `aviary test` so paths work from the repo root too.
	Workdir string `yaml:"workdir"`
	// Env holds extra environment variables passed to the suite's command
	// after `${VAR}` interpolation against the stack's environment.
	Env map[string]string `yaml:"env"`
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

// WantsPrepare reports whether the suite should run the host prepare steps first.
func (s Suite) WantsPrepare() bool {
	return s.Prepare == nil || *s.Prepare
}

// UsesContainers reports whether running the suite starts or runs any Docker
// container. Pure host suites (`host: true`, `init: false`, no `needs`) never
// touch Docker.
func (s Suite) UsesContainers() bool {
	return s.WantsInit() || len(s.Needs) > 0 || !s.Host
}

// IdleDuration returns the configured idle TTL or DefaultIdleTTL when unset.
func (c *Config) IdleDuration() time.Duration {
	if c.IdleTTL == "" {
		return DefaultIdleTTL
	}
	d, err := time.ParseDuration(c.IdleTTL)
	if err != nil || d <= 0 {
		return DefaultIdleTTL
	}
	return d
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
	if err := noEmpty("compose.files", c.Compose.Files); err != nil {
		return err
	}
	if err := noEmpty("compose.profiles", c.Compose.Profiles); err != nil {
		return err
	}
	if c.IdleTTL != "" {
		d, err := time.ParseDuration(c.IdleTTL)
		if err != nil || d <= 0 {
			return fmt.Errorf("idle_ttl %q must be a positive Go duration (such as 20m)", c.IdleTTL)
		}
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
	if c.Vault != nil {
		if err := c.Vault.validate(); err != nil {
			return fmt.Errorf("vault: %w", err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.HostEnv)) {
		if !envName.MatchString(name) {
			return fmt.Errorf("host_env name %q must match %s", name, envName)
		}
		if err := c.HostEnv[name].validate(); err != nil {
			return fmt.Errorf("host_env[%q]: %w", name, err)
		}
	}
	for i, step := range c.Prepare {
		if err := step.validate(); err != nil {
			return fmt.Errorf("prepare[%d]: %w", i, err)
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
	for _, name := range slices.Sorted(maps.Keys(c.Suites)) {
		if err := c.Suites[name].validate(); err != nil {
			return fmt.Errorf("suite %q: %w", name, err)
		}
	}
	if c.Snapshot != nil {
		if err := c.Snapshot.validate(); err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
	}
	return nil
}

func (s Snapshot) validate() error {
	if len(s.Volumes) == 0 {
		return fmt.Errorf("volumes must name at least one volume")
	}
	if err := noEmpty("volumes", s.Volumes); err != nil {
		return err
	}
	if err := validRelativePaths("key_files", s.KeyFiles); err != nil {
		return err
	}
	return validRelativePaths("stamp_files", s.StampFiles)
}

func validRelativePaths(field string, paths []string) error {
	for i, p := range paths {
		if p == "" {
			return fmt.Errorf("%s[%d] is empty", field, i)
		}
		if !filepath.IsLocal(p) {
			return fmt.Errorf("%s[%d] %q must be relative to the worktree root", field, i, p)
		}
	}
	return nil
}

func (v Vault) validate() error {
	if v.Addr == "" {
		return fmt.Errorf("needs addr")
	}
	u, err := url.Parse(v.Addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("addr %q must be an http or https URL", v.Addr)
	}
	if v.TTL != "" {
		d, err := time.ParseDuration(v.TTL)
		if err != nil || d <= 0 {
			return fmt.Errorf("ttl %q must be a positive Go duration (such as 8h)", v.TTL)
		}
	}
	return nil
}

func (f HostFile) validate() error {
	if f.Path == "" {
		return fmt.Errorf("needs path")
	}
	return nil
}

func (s HostStep) validate() error {
	if len(s.Command) == 0 {
		return fmt.Errorf("needs a command")
	}
	return noEmpty("command", s.Command)
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
	if s.Host {
		if s.Service != "" {
			return fmt.Errorf("host suite must not set service")
		}
		if len(s.Command) == 0 {
			return fmt.Errorf("needs a command")
		}
		if err := noEmpty("command", s.Command); err != nil {
			return err
		}
	} else {
		if err := (Step{Service: s.Service, Command: s.Command}).validate(); err != nil {
			return err
		}
	}
	if s.Workdir != "" && s.Workdir != "." && !filepath.IsLocal(s.Workdir) {
		return fmt.Errorf("workdir %q must be relative to the worktree root", s.Workdir)
	}
	for _, name := range slices.Sorted(maps.Keys(s.Env)) {
		if !envName.MatchString(name) {
			return fmt.Errorf("env name %q must match %s", name, envName)
		}
	}
	if err := noEmpty("needs", s.Needs); err != nil {
		return err
	}
	return noEmpty("profiles", s.Profiles)
}

// patternValueFlags are common test-runner flags whose following argument is a
// filter expression rather than a file path.
var patternValueFlags = map[string]bool{
	"-e": true, "--example": true,
	"-g": true, "--grep": true,
	"-k": true, "-m": true, "-t": true,
}

// NormalizeArgs rewrites path arguments in extra that point inside s.Workdir
// (from root, cwd, or an absolute path) so they are relative to s.Workdir.
// Arguments already relative to s.Workdir, flags, and filter patterns are
// returned unchanged.
func (s Suite) NormalizeArgs(root, cwd string, extra []string) []string {
	workdir := strings.TrimSuffix(filepath.ToSlash(filepath.Clean(s.Workdir)), "/")
	if s.Workdir == "" || workdir == "." {
		return extra
	}
	suiteDir := filepath.Join(root, filepath.FromSlash(workdir))
	out := make([]string, len(extra))
	skipNext := false
	for i, arg := range extra {
		if skipNext {
			out[i] = arg
			skipNext = false
			continue
		}
		if strings.HasPrefix(arg, "-") {
			out[i] = arg
			skipNext = patternValueFlags[arg]
			continue
		}
		out[i] = normalizeSuiteArg(suiteDir, cwd, workdir, arg)
	}
	return out
}

func normalizeSuiteArg(suiteDir, cwd, workdir, arg string) string {
	if arg == "" || strings.Contains(arg, "://") {
		return arg
	}
	pathPart, suffix := splitSelector(arg)
	if pathPart == "" {
		return arg
	}
	if pathExists(filepath.Join(suiteDir, pathPart)) {
		return arg
	}
	if filepath.IsAbs(pathPart) {
		if rel, ok := relWithin(suiteDir, pathPart); ok {
			return filepath.ToSlash(rel) + suffix
		}
		return arg
	}
	if cwd != "" && cwd != suiteDir {
		candidate := filepath.Join(cwd, pathPart)
		if pathExists(candidate) {
			if rel, ok := relWithin(suiteDir, candidate); ok {
				return filepath.ToSlash(rel) + suffix
			}
		}
	}
	trimmed := strings.TrimPrefix(filepath.ToSlash(pathPart), "./")
	if trimmed == workdir || trimmed == workdir+"/" {
		return "." + suffix
	}
	if strings.HasPrefix(trimmed, workdir+"/") {
		return strings.TrimPrefix(trimmed, workdir+"/") + suffix
	}
	return arg
}

func splitSelector(arg string) (pathPart, suffix string) {
	idx := strings.IndexByte(arg, ':')
	if idx <= 0 {
		return arg, ""
	}
	return arg[:idx], arg[idx:]
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func relWithin(base, target string) (string, bool) {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return "", false
	}
	if rel == "." {
		return ".", true
	}
	if !filepath.IsLocal(rel) {
		return "", false
	}
	return rel, true
}

func noEmpty(field string, values []string) error {
	for i, v := range values {
		if v == "" {
			return fmt.Errorf("%s[%d] is empty", field, i)
		}
	}
	return nil
}
