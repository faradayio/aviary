// Package stack assembles one worktree's stack from its definition, its
// identity and its host ports, and runs the lifecycle on top of compose:
// seeding with init steps, running test suites, and taking the stack down.
// It is the only package that composes the others.
package stack

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/faradayio/aviary/internal/compose"
	"github.com/faradayio/aviary/internal/config"
	"github.com/faradayio/aviary/internal/registry"
	"github.com/faradayio/aviary/internal/workspace"
)

// Runner executes one compose subcommand against a stack. compose.Stack
// satisfies it.
type Runner interface {
	Run(args ...string) error
}

type Stack struct {
	Config    *config.Config
	Workspace workspace.Workspace
	// Entry is this stack's registry entry: its host ports and whether its
	// volumes have been seeded.
	Entry    registry.Entry
	registry *registry.Registry
	// runner turns a compose invocation into something that can run it.
	// Tests replace it to record argv instead of calling Docker.
	runner func(compose.Stack) Runner
}

// Options adjusts how Load builds a stack.
type Options struct {
	// SkipRequireEnv loads the stack without checking require_env, for
	// commands that never start or run a container and so never hand the
	// required variables to one.
	SkipRequireEnv bool
	// Registry is where ports are allocated. Nil means the machine-wide
	// registry from registry.Open.
	Registry *registry.Registry
}

// Load builds the stack for the worktree containing cwd, allocating any host
// ports it does not have yet. configDir is the external definition directory
// from --config or AVIARY_CONFIG, or empty when neither is set. Unless
// opts.SkipRequireEnv is set, every name in require_env must be set in the
// host environment; otherwise Load fails listing all the missing names,
// before touching the registry.
func Load(cwd, configDir string, opts Options) (*Stack, error) {
	reg := opts.Registry
	if reg == nil {
		var err error
		if reg, err = registry.Open(); err != nil {
			return nil, err
		}
	}
	ws, err := workspace.Detect(cwd, configDir)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(ws.Definition)
	if err != nil {
		return nil, err
	}
	if !opts.SkipRequireEnv {
		if err := checkRequiredEnv(cfg.RequireEnv); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(ws.Definition, config.FileName), err)
		}
	}
	s := &Stack{Config: cfg, Workspace: ws, registry: reg, runner: composeRunner}
	entry, err := reg.Allocate(ws.Project(), ws.Root, s.configSource(), cfg.Ports)
	if err != nil {
		return nil, err
	}
	s.Entry = entry
	return s, nil
}

func composeRunner(c compose.Stack) Runner { return c }

// checkRequiredEnv treats an empty variable as missing: an empty credential
// fails the same way an unset one does, only later and less clearly.
func checkRequiredEnv(names []string) error {
	var missing []string
	for _, name := range names {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("require_env: not set in the environment: %s", strings.Join(missing, ", "))
	}
	return nil
}

// configSource is what the registry records as the definition's location:
// the definition directory when it is external, empty when it is in-tree.
func (s *Stack) configSource() string {
	if s.Workspace.External {
		return s.Workspace.Definition
	}
	return ""
}

// Env is the environment aviary adds for compose interpolation and for
// processes that want to reach the stack from the host.
func (s *Stack) Env() map[string]string {
	env := map[string]string{
		"COMPOSE_PROJECT_NAME": s.Workspace.Project(),
		"AVIARY_PROJECT":       s.Workspace.Project(),
		"AVIARY_SLUG":          s.Workspace.Slug,
		"AVIARY_ROOT":          s.Workspace.Root,
	}
	for name, port := range s.Entry.Ports {
		env["AVIARY_PORT_"+strings.ToUpper(name)] = strconv.Itoa(port)
	}
	return env
}

// EnvLines renders Env as KEY=VALUE lines sorted by key.
func (s *Stack) EnvLines() []string {
	env := s.Env()
	lines := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		lines = append(lines, k+"="+env[k])
	}
	return lines
}

// Compose is the compose invocation for this stack with the stack-wide
// profiles.
func (s *Stack) Compose() compose.Stack {
	return compose.Stack{
		Project:    s.Workspace.Project(),
		Root:       s.Workspace.Root,
		Definition: s.Workspace.Definition,
		Files:      s.Config.Compose.Files,
		Profiles:   s.Config.Compose.Profiles,
		Env:        s.Env(),
	}
}

// run executes a compose subcommand with the stack-wide profiles plus
// extraProfiles.
func (s *Stack) run(extraProfiles []string, args ...string) error {
	c := s.Compose()
	c.Profiles = slices.Concat(c.Profiles, extraProfiles)
	return s.runner(c).Run(args...)
}

// Init seeds the stack's volumes by running each init step in a fresh
// container, in order. Unless force is set, it is a no-op once that has
// happened for the current volumes. A failing step stops the sequence and
// leaves the stack uninitialized, with the partial state kept for
// inspection.
func (s *Stack) Init(force bool) error {
	if s.Entry.InitializedAt != nil && !force {
		return nil
	}
	if err := s.runSteps("init", s.Config.Init); err != nil {
		return err
	}
	return s.markInitialized(true)
}

// runSteps runs steps in order and stops at the first failure. The error
// names the step and is deliberately not a *compose.ExitError, so the CLI
// reports it as aviary's own failure instead of exiting silently with the
// step's status.
func (s *Stack) runSteps(phase string, steps []config.Step) error {
	for i, step := range steps {
		desc := step.Service + " " + strings.Join(step.Command, " ")
		fmt.Fprintf(os.Stderr, "aviary: %s: %s\n", phase, desc)
		args := append([]string{"run", "--rm", step.Service}, step.Command...)
		if err := s.run(nil, args...); err != nil {
			return fmt.Errorf("%s step %d (%s) failed: %v", phase, i+1, desc, err)
		}
	}
	return nil
}

// Test runs the named suite: init first if the suite wants it, then its
// needs brought up and healthy, then the suite's command in a fresh
// container with extra appended (a leading "--" is dropped). The stack-wide
// and suite profiles apply to both compose calls. A suite that fails returns
// its status as a *compose.ExitError.
func (s *Stack) Test(name string, extra []string) error {
	suite, ok := s.Config.Suites[name]
	if !ok {
		names := s.SuiteNames()
		if len(names) == 0 {
			return fmt.Errorf("no suite %q; this stack defines no suites", name)
		}
		return fmt.Errorf("no suite %q; available: %s", name, strings.Join(names, ", "))
	}
	if suite.WantsInit() {
		if err := s.Init(false); err != nil {
			return err
		}
	}
	if len(suite.Needs) > 0 {
		args := append([]string{"up", "--detach", "--wait"}, suite.Needs...)
		if err := s.run(suite.Profiles, args...); err != nil {
			return fmt.Errorf("starting %s for suite %q: %v", strings.Join(suite.Needs, ", "), name, err)
		}
	}
	if len(extra) > 0 && extra[0] == "--" {
		extra = extra[1:]
	}
	args := slices.Concat([]string{"run", "--rm", suite.Service}, suite.Command, extra)
	return s.run(suite.Profiles, args...)
}

// SuiteNames returns the stack's suite names, sorted.
func (s *Stack) SuiteNames() []string {
	return slices.Sorted(maps.Keys(s.Config.Suites))
}

// Down stops the stack. With volumes, its data goes too and the stack runs
// init again next time.
func (s *Stack) Down(volumes bool) error {
	args := []string{"down", "--remove-orphans"}
	if volumes {
		args = append(args, "--volumes")
	}
	if err := s.run(nil, args...); err != nil {
		return err
	}
	if volumes {
		return s.markInitialized(false)
	}
	return nil
}

// Remove takes the stack down with its volumes and then forgets it,
// releasing its host ports. When compose fails the entry is kept, so the
// stack stays visible to ls and gc.
func (s *Stack) Remove() error {
	if err := s.Down(true); err != nil {
		return err
	}
	return s.registry.Release(s.Workspace.Project())
}

// markInitialized records done in the registry and reloads Entry from it.
// Allocate with unchanged ports returns the entry without probing.
func (s *Stack) markInitialized(done bool) error {
	if err := s.registry.SetInitialized(s.Workspace.Project(), done); err != nil {
		return err
	}
	entry, err := s.registry.Allocate(s.Workspace.Project(), s.Workspace.Root, s.configSource(), s.Config.Ports)
	if err != nil {
		return err
	}
	s.Entry = entry
	return nil
}
