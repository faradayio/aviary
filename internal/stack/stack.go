// Package stack assembles one worktree's stack from its definition, its
// identity and its host ports, and runs the lifecycle on top of compose:
// seeding with init steps, running test suites, and taking the stack down.
// It is the only package that composes the others.
package stack

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/faradayio/aviary/internal/compose"
	"github.com/faradayio/aviary/internal/config"
	"github.com/faradayio/aviary/internal/dockerdesktop"
	"github.com/faradayio/aviary/internal/registry"
	"github.com/faradayio/aviary/internal/snapshot"
	"github.com/faradayio/aviary/internal/vault"
	"github.com/faradayio/aviary/internal/workspace"
)

type Stack struct {
	Config    *config.Config
	Workspace workspace.Workspace
	// Cwd is the caller's working directory, used to rewrite repo-relative
	// test path arguments against a suite's Workdir.
	Cwd string
	// Entry is this stack's registry entry: its host ports and whether
	// aviary has recorded that its init steps ran. It is the zero Entry,
	// with no ports, when Load ran with NoAllocate for a stack that is not
	// registered.
	Entry    registry.Entry
	registry *registry.Registry
	// resolvedEnv holds credentials and host file paths resolved during
	// Load (Vault token, host_env paths), added to compose invocations
	// without appearing in `aviary env` output.
	resolvedEnv map[string]string
	// runner is Options.Runner.
	runner func(c compose.Stack, args ...string) error
	// resolver is Options.Resolver.
	resolver func(c compose.Stack) (*compose.Resolved, error)
	// hostRunner is Options.HostRunner.
	hostRunner func(dir string, extraEnv map[string]string, command []string) error
	// spawnReaper is Options.SpawnReaper.
	spawnReaper func(project string)
	// snapshots caches seeded volumes when Config.Snapshot is set.
	snapshots *snapshot.Store
	// dockerSettings holds local Docker Desktop settings on Apple silicon.
	dockerSettings *dockerdesktop.Settings
	warnedDocker   bool
	// prepared is set once this Stack has run the prepare steps.
	prepared bool
	// inUse is set while this Stack holds a UseLease so nested calls share it.
	inUse bool
	// stderr receives aviary's progress lines for init steps.
	stderr io.Writer
}

// Options adjusts how Load builds a stack.
type Options struct {
	// SkipRequireEnv loads the stack without resolving vault/host_env or
	// checking require_env, for commands that never start or run a
	// container and so never hand the required variables to one.
	SkipRequireEnv bool
	// NoAllocate reads the stack's registry entry instead of allocating,
	// for commands that only inspect or stop a stack and so must not
	// register one. An unregistered stack gets no ports, and its compose
	// files fall back to their own defaults.
	NoAllocate bool
	// Registry is where ports are allocated. It is required.
	Registry *registry.Registry
	// Runner runs one compose subcommand against a compose invocation. It
	// defaults to compose.Stack.Run; tests replace it to record argv
	// instead of calling Docker.
	Runner func(c compose.Stack, args ...string) error
	// Resolver resolves `docker compose config --format json` for snapshot
	// identity calculation. Defaults to compose.Stack.Config.
	Resolver func(c compose.Stack) (*compose.Resolved, error)
	// HostRunner runs a command on the host in dir. It defaults to
	// compose.RunHost; tests replace it to record host commands.
	HostRunner func(dir string, extraEnv map[string]string, command []string) error
	// SpawnReaper starts a detached background idle reaper for project when a
	// warm stack is used. Nil disables spawning (for unit tests).
	SpawnReaper func(project string)
	// SnapshotStore overrides the default XDG snapshot cache during tests.
	SnapshotStore *snapshot.Store
	// VaultToken overrides Vault token minting during tests. When nil and
	// cfg.Vault is set, vault.NewClient(cfg.Vault).Token is called.
	VaultToken func(cfg *config.Vault) (string, error)
}

// Load builds the stack for the worktree containing cwd, allocating any host
// ports it does not have yet unless opts.NoAllocate is set. configDir is the
// external definition directory from --config or AVIARY_CONFIG, or empty when
// neither is set. Unless opts.SkipRequireEnv is set, vault, host_env and
// require_env are resolved and checked before touching the registry.
func Load(cwd, configDir string, opts Options) (*Stack, error) {
	ws, err := workspace.Detect(cwd, configDir)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(ws.Definition)
	if err != nil {
		return nil, err
	}
	defFile := filepath.Join(ws.Definition, config.FileName)
	resolvedEnv := map[string]string{}
	if !opts.SkipRequireEnv {
		if cfg.Vault != nil {
			mint := opts.VaultToken
			if mint == nil {
				mint = defaultVaultToken
			}
			token, err := mint(cfg.Vault)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", defFile, err)
			}
			resolvedEnv["VAULT_ADDR"] = cfg.Vault.Addr
			resolvedEnv["VAULT_TOKEN"] = token
		}
		if err := resolveHostEnv(cfg.HostEnv, resolvedEnv); err != nil {
			return nil, fmt.Errorf("%s: %w", defFile, err)
		}
		if err := checkRequiredEnv(cfg.RequireEnv, resolvedEnv); err != nil {
			return nil, fmt.Errorf("%s: %w", defFile, err)
		}
	}
	reg := opts.Registry
	var entry registry.Entry
	if opts.NoAllocate {
		entry, _, err = reg.Get(ws.Project())
	} else {
		// The registry records an external definition's directory, and
		// nothing for an in-tree one.
		source := ""
		if ws.External {
			source = ws.Definition
		}
		entry, err = reg.Allocate(ws.Project(), ws.Root, source, cfg.Ports)
	}
	if err != nil {
		return nil, err
	}
	runner := opts.Runner
	if runner == nil {
		runner = compose.Stack.Run
	}
	resolver := opts.Resolver
	if resolver == nil {
		resolver = compose.Stack.Config
	}
	hostRunner := opts.HostRunner
	if hostRunner == nil {
		hostRunner = compose.RunHost
	}
	callerDir := cwd
	if abs, err := filepath.Abs(cwd); err == nil {
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			callerDir = resolved
		} else {
			callerDir = abs
		}
	}
	var dockerSettings *dockerdesktop.Settings
	if !opts.SkipRequireEnv {
		dockerSettings = dockerdesktop.LoadDefault()
	}
	return &Stack{
		Config:         cfg,
		Workspace:      ws,
		Cwd:            callerDir,
		Entry:          entry,
		registry:       reg,
		resolvedEnv:    resolvedEnv,
		runner:         runner,
		resolver:       resolver,
		hostRunner:     hostRunner,
		spawnReaper:    opts.SpawnReaper,
		snapshots:      opts.SnapshotStore,
		dockerSettings: dockerSettings,
		stderr:         os.Stderr,
	}, nil
}

// WarnDockerDesktop prints one-line warnings to stderr at most once when
// linux/amd64 compose services run on Apple silicon without Rosetta or enough
// VM memory.
func (s *Stack) WarnDockerDesktop() {
	if s.warnedDocker || s.dockerSettings == nil {
		return
	}
	s.warnedDocker = true
	hasAmd64 := dockerdesktop.ComposeFilesSpecifyLinuxAmd64(s.Workspace.Definition, s.Config.Compose.Files)
	for _, w := range s.dockerSettings.Warnings(hasAmd64) {
		fmt.Fprintf(s.stderr, "aviary: warning: %s\n", w)
	}
}

func defaultVaultToken(cfg *config.Vault) (string, error) {
	client, err := vault.NewClient(cfg)
	if err != nil {
		return "", err
	}
	return client.Token()
}

func resolveHostEnv(specs map[string]config.HostFile, out map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(specs)) {
		if os.Getenv(name) != "" || out[name] != "" {
			continue
		}
		spec := specs[name]
		expanded, err := expandHome(spec.Path)
		if err != nil {
			return err
		}
		if info, err := os.Stat(expanded); err == nil && !info.IsDir() {
			out[name] = expanded
			continue
		}
		if spec.Required {
			msg := fmt.Sprintf("host_env %s: %s does not exist", name, expanded)
			if spec.Hint != "" {
				msg += " (" + spec.Hint + ")"
			}
			return fmt.Errorf("%s", msg)
		}
	}
	return nil
}

func expandHome(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

// checkRequiredEnv treats an empty variable as missing: an empty credential
// fails the same way an unset one does, only later and less clearly.
func checkRequiredEnv(names []string, resolved map[string]string) error {
	var missing []string
	for _, name := range names {
		if os.Getenv(name) == "" && resolved[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("require_env: not set in the environment: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Env is the public environment aviary adds for compose interpolation and for
// processes that want to reach the stack from the host (`aviary env`). Secrets
// such as VAULT_TOKEN are deliberately excluded so `aviary env` never prints
// credentials.
func (s *Stack) Env() map[string]string {
	env := map[string]string{
		"COMPOSE_PROJECT_NAME": s.Workspace.Project(),
		"AVIARY_PROJECT":       s.Workspace.Project(),
		"AVIARY_SLUG":          s.Workspace.Slug,
		"AVIARY_ROOT":          s.Workspace.Root,
		// The definition's directory, so a compose file can reach files
		// kept next to an external aviary.yml.
		"AVIARY_CONFIG_DIR": s.Workspace.Definition,
	}
	for name, port := range s.Entry.Ports {
		env["AVIARY_PORT_"+strings.ToUpper(name)] = strconv.Itoa(port)
	}
	return env
}

// composeEnv is Env plus the resolved credentials, plus extra with each value
// interpolated against those (not against other extra values) and then the
// host environment. `$$` is a literal `$`, as in compose files.
func (s *Stack) composeEnv(extra map[string]string) map[string]string {
	base := s.Env()
	maps.Copy(base, s.resolvedEnv)
	env := maps.Clone(base)
	for k, v := range extra {
		env[k] = os.Expand(v, func(name string) string {
			if name == "$" {
				return "$"
			}
			if val, ok := base[name]; ok {
				return val
			}
			return os.Getenv(name)
		})
	}
	return env
}

// EnvLines renders Env as KEY=VALUE lines sorted by key.
func (s *Stack) EnvLines() []string {
	return s.envLines(func(k, v string) string { return k + "=" + v })
}

// ExportLines renders Env as `export KEY='VALUE'` lines sorted by key. Each
// value is single-quoted, and a quote inside it closes the quoting, adds an
// escaped quote and reopens, so a shell that evals the lines takes roots
// holding spaces, $, quotes or parentheses literally.
func (s *Stack) ExportLines() []string {
	return s.envLines(func(k, v string) string {
		return "export " + k + "='" + strings.ReplaceAll(v, "'", `'\''`) + "'"
	})
}

func (s *Stack) envLines(line func(k, v string) string) []string {
	env := s.Env()
	lines := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		lines = append(lines, line(k, env[k]))
	}
	return lines
}

// Compose is the compose invocation for this stack with the stack-wide
// profiles and any resolved credentials.
func (s *Stack) Compose() compose.Stack {
	return compose.Stack{
		Project:    s.Workspace.Project(),
		Root:       s.Workspace.Root,
		Definition: s.Workspace.Definition,
		Files:      s.Config.Compose.Files,
		Profiles:   s.Config.Compose.Profiles,
		Env:        s.composeEnv(nil),
	}
}

// run executes a compose subcommand with the stack-wide profiles plus
// extraProfiles.
func (s *Stack) run(extraProfiles []string, args ...string) error {
	c := s.Compose()
	c.Profiles = slices.Concat(c.Profiles, extraProfiles)
	return s.runner(c, args...)
}

// Up starts the stack (or the named services) and waits until healthy. Held
// is set only after that succeeds. A failed up starts the idle timer, unless
// the stack is already held. A held stack stays up until Down or rm.
func (s *Stack) Up(args []string) error {
	lease, err := s.registry.BeginHold(s.Workspace.Project())
	if err != nil {
		return err
	}
	defer lease.Finish()
	s.Entry = lease.Entry
	s.WarnDockerDesktop()
	if err := s.Prepare(); err != nil {
		return err
	}
	if err := s.run(nil, append([]string{"up", "--detach", "--wait"}, args...)...); err != nil {
		return s.markUsed(err)
	}
	entry, err := s.registry.MarkHeld(s.Workspace.Project())
	if err != nil {
		return err
	}
	s.Entry = entry
	return nil
}

// RunOneOff runs a one-off command in a fresh container (`aviary run`), marking
// the stack warm so any dependency services it started cool down after idle_ttl.
func (s *Stack) RunOneOff(args []string) error {
	return s.withUse(func() error {
		s.WarnDockerDesktop()
		if err := s.Prepare(); err != nil {
			return err
		}
		return s.run(nil, append([]string{"run", "--rm"}, args...)...)
	})
}

// ComposeHold runs `aviary compose up`. The stack is marked held only after
// the command succeeds, same as Up.
func (s *Stack) ComposeHold(args []string) error {
	lease, err := s.registry.BeginHold(s.Workspace.Project())
	if err != nil {
		return err
	}
	defer lease.Finish()
	s.Entry = lease.Entry
	s.WarnDockerDesktop()
	if err := s.run(nil, args...); err != nil {
		return s.markUsed(err)
	}
	entry, err := s.registry.MarkHeld(s.Workspace.Project())
	if err != nil {
		return err
	}
	s.Entry = entry
	return nil
}

// markUsed starts the idle timer after a start command fails. A stack that
// is already held stays held, so its containers are not reaped.
func (s *Stack) markUsed(cause error) error {
	updated, err := s.registry.MarkUsed(s.Workspace.Project(), s.Config.IdleTTL)
	if err != nil {
		return errors.Join(cause, err)
	}
	if updated == nil {
		return cause
	}
	s.Entry = *updated
	if updated.IsWarm() && s.spawnReaper != nil {
		s.spawnReaper(s.Workspace.Project())
	}
	return cause
}

// ComposeUse forwards an `aviary compose run` invocation under a transient UseLease.
func (s *Stack) ComposeUse(args []string) error {
	return s.withUse(func() error {
		s.WarnDockerDesktop()
		return s.run(nil, args...)
	})
}

func (s *Stack) withUse(fn func() error) (err error) {
	if s.inUse {
		return fn()
	}
	lease, err := s.registry.BeginUse(s.Workspace.Project(), s.Config.IdleTTL)
	if err != nil {
		return err
	}
	s.Entry = lease.Entry
	s.inUse = true
	defer func() {
		s.inUse = false
		updated, finishErr := lease.Finish()
		if finishErr == nil && updated != nil {
			s.Entry = *updated
			if updated.IsWarm() && s.spawnReaper != nil {
				s.spawnReaper(s.Workspace.Project())
			}
		}
		if err == nil {
			err = finishErr
		}
	}()
	err = fn()
	return err
}

// Prepare runs the host prepare steps from aviary.yml in the worktree root,
// under the stack's lock so two aviary commands in one worktree never run
// them at once.
func (s *Stack) Prepare() error {
	unlock, err := s.registry.LockProject(s.Workspace.Project())
	if err != nil {
		return err
	}
	defer unlock()
	return s.prepareLocked()
}

// prepareLocked runs the prepare steps at most once per Stack, so a command
// that prepares and then inits runs them once. The caller holds the lock.
func (s *Stack) prepareLocked() error {
	if s.prepared {
		return nil
	}
	for i, step := range s.Config.Prepare {
		desc := strings.Join(step.Command, " ")
		if err := s.hostRunner(s.Workspace.Root, s.composeEnv(nil), step.Command); err != nil {
			return fmt.Errorf("prepare step %d (%s) failed: %v", i+1, desc, err)
		}
	}
	s.prepared = true
	return nil
}

// Init seeds the stack's volumes by running each init step in a fresh
// container, in order. Unless force is set, it is a no-op once init has run
// since the stack was created or since aviary last removed its volumes
// (`down --volumes`, `compose down --volumes` or `rm`). A failing step stops the sequence and
// leaves the stack uninitialized, with the partial state kept for
// inspection. It holds the stack's lock throughout, so a second aviary in the
// same worktree waits and then finds init done instead of running it again.
func (s *Stack) Init(force bool) error {
	if !s.inUse && !force {
		done, err := s.alreadyInitialized()
		if err != nil || done {
			return err
		}
	}
	return s.withUse(func() error {
		return s.initLocked(force)
	})
}

func (s *Stack) alreadyInitialized() (bool, error) {
	unlock, err := s.registry.LockProject(s.Workspace.Project())
	if err != nil {
		return false, err
	}
	defer unlock()
	if err := s.reload(); err != nil {
		return false, err
	}
	return s.Entry.InitializedAt != nil, nil
}

func (s *Stack) initLocked(force bool) error {
	unlock, err := s.registry.LockProject(s.Workspace.Project())
	if err != nil {
		return err
	}
	defer unlock()
	// Another aviary may have run init while this one waited for the lock.
	if err := s.reload(); err != nil {
		return err
	}
	if s.Entry.InitializedAt != nil && !force {
		return nil
	}
	s.WarnDockerDesktop()
	if err := s.prepareLocked(); err != nil {
		return err
	}
	if s.Config.Snapshot != nil {
		return s.initWithSnapshot(force)
	}
	if err := s.runSteps("init", s.Config.Init); err != nil {
		return err
	}
	return s.markInitialized(true)
}

func (s *Stack) initWithSnapshot(force bool) error {
	resolveCompose := s.Compose()
	resolveCompose.Profiles = []string{"*"}
	resolveFn := s.resolver
	if resolveFn == nil {
		resolveFn = compose.Stack.Config
	}
	resolved, err := resolveFn(resolveCompose)
	if err != nil {
		return fmt.Errorf("resolving compose config for snapshot: %w", err)
	}
	id, err := snapshot.BuildIdentity(s.Workspace.Project(), s.Workspace.Root, s.Config.Snapshot, s.Config.Init, resolved)
	if err != nil {
		return err
	}
	store := s.snapshots
	if store == nil {
		store, err = snapshot.OpenDefault()
		if err != nil {
			return err
		}
	}

	if !force {
		hit, err := store.Lookup(id, len(s.Config.AfterRestore) > 0)
		if err != nil {
			return err
		}
		if hit != nil {
			restoreStart := time.Now()
			if hit.Exact {
				fmt.Fprintf(s.stderr, "aviary: init: restoring snapshot %s\n", hit.Stamp)
			} else {
				fmt.Fprintf(s.stderr, "aviary: init: restoring snapshot %s (schema changed; after_restore will run)\n", hit.Stamp)
			}
			if err := s.run(nil, "down", "--remove-orphans"); err != nil {
				return fmt.Errorf("stopping stack before snapshot restore: %v", err)
			}
			if err := store.Restore(id, hit.Stamp); err != nil {
				return err
			}
			fmt.Fprintf(s.stderr, "aviary: init: snapshot restored in %s\n", seconds(time.Since(restoreStart)))
			if !hit.Exact {
				if err := s.runSteps("after_restore", s.Config.AfterRestore); err != nil {
					return err
				}
				if err := s.checkpointAndSaveSnapshot(store, id); err != nil {
					return err
				}
			}
			return s.markInitialized(true)
		}
	}

	if err := s.runSteps("init", s.Config.Init); err != nil {
		return err
	}
	if err := s.checkpointAndSaveSnapshot(store, id); err != nil {
		return err
	}
	return s.markInitialized(true)
}

func (s *Stack) checkpointAndSaveSnapshot(store *snapshot.Store, id *snapshot.Identity) error {
	if err := s.run(nil, "down", "--remove-orphans"); err != nil {
		return fmt.Errorf("stopping stack before saving snapshot: %v", err)
	}
	saveStart := time.Now()
	if err := store.Save(id); err != nil {
		return err
	}
	fmt.Fprintf(s.stderr, "aviary: init: saved snapshot %s in %s\n", id.Stamp, seconds(time.Since(saveStart)))
	return nil
}

// runSteps runs steps in order and stops at the first failure. It reports
// each step as it starts, and its wall-clock time and the total as they
// succeed. The error names the step and is deliberately not a
// *compose.ExitError, so the CLI reports it as aviary's own failure instead
// of exiting silently with the step's status.
func (s *Stack) runSteps(phase string, steps []config.Step) error {
	start := time.Now()
	for i, step := range steps {
		desc := step.Service + " " + strings.Join(step.Command, " ")
		fmt.Fprintf(s.stderr, "aviary: %s: %s\n", phase, desc)
		stepStart := time.Now()
		args := append([]string{"run", "--rm", step.Service}, step.Command...)
		if err := s.run(nil, args...); err != nil {
			return fmt.Errorf("%s step %d (%s) failed: %v", phase, i+1, desc, err)
		}
		fmt.Fprintf(s.stderr, "aviary: %s: step %d/%d ok %s\n", phase, i+1, len(steps), seconds(time.Since(stepStart)))
	}
	if len(steps) > 0 {
		fmt.Fprintf(s.stderr, "aviary: %s: total %s\n", phase, seconds(time.Since(start)))
	}
	return nil
}

// seconds renders d as seconds to one decimal place, such as 2.1s.
func seconds(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// Test runs the named suite: prepare steps if wanted, init if wanted, then its
// needs brought up and healthy, then the suite's command in a fresh container
// (or on the host when suite.Host is true) with extra appended (a leading "--"
// is dropped). The stack-wide and suite profiles apply to both compose calls.
// A suite that fails returns its status as a *compose.ExitError.
//
// The first thing it prints is whether init will run. A first init can sit
// for several minutes before any other output.
func (s *Stack) Test(name string, extra []string) error {
	suite, ok := s.Config.Suites[name]
	if !ok {
		names := s.SuiteNames()
		if len(names) == 0 {
			return fmt.Errorf("no suite %q; this stack defines no suites", name)
		}
		return fmt.Errorf("no suite %q; available: %s", name, strings.Join(names, ", "))
	}
	s.announceTest(name, suite)
	runSuite := func() error {
		if suite.WantsPrepare() {
			if err := s.Prepare(); err != nil {
				return err
			}
		}
		if suite.WantsInit() {
			if err := s.Init(false); err != nil {
				return err
			}
		}
		if len(suite.Needs) > 0 || !suite.Host {
			s.WarnDockerDesktop()
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
		callerDir := s.Cwd
		if callerDir == "" {
			callerDir = s.Workspace.Root
		}
		extra = suite.NormalizeArgs(s.Workspace.Root, callerDir, extra)
		if suite.Host {
			dir := s.Workspace.Root
			if suite.Workdir != "" {
				dir = filepath.Join(dir, suite.Workdir)
			}
			return s.hostRunner(dir, s.composeEnv(suite.Env), slices.Concat(suite.Command, extra))
		}
		// `--env KEY` with no value makes compose read the value from its own
		// environment, so values never appear in the process list.
		envFlags := make([]string, 0, len(suite.Env)*2)
		for _, k := range slices.Sorted(maps.Keys(suite.Env)) {
			envFlags = append(envFlags, "--env", k)
		}
		c := s.Compose()
		c.Profiles = slices.Concat(c.Profiles, suite.Profiles)
		c.Env = s.composeEnv(suite.Env)
		return s.runner(c, slices.Concat([]string{"run", "--rm"}, envFlags, []string{suite.Service}, suite.Command, extra)...)
	}
	if suite.UsesContainers() {
		return s.withUse(runSuite)
	}
	return runSuite()
}

// announceTest names the suite and says whether init will run, before prepare
// or init, so a quiet first run is not mistaken for a hang.
func (s *Stack) announceTest(name string, suite config.Suite) {
	if suite.WantsInit() && s.Entry.InitializedAt == nil {
		fmt.Fprintf(s.stderr, "aviary: test %s: init running (the first init in a worktree can take several minutes)\n", name)
		return
	}
	fmt.Fprintf(s.stderr, "aviary: test %s: init skipped\n", name)
}

// SuiteNames returns the stack's suite names, sorted.
func (s *Stack) SuiteNames() []string {
	return slices.Sorted(maps.Keys(s.Config.Suites))
}

// Down stops the stack. With volumes, its data goes too and the stack runs
// init again next time. A stack that is not registered stays unregistered.
func (s *Stack) Down(volumes bool) error {
	args := []string{"down", "--remove-orphans"}
	if !volumes {
		if err := s.run(nil, args...); err != nil {
			return err
		}
		return s.MarkStopped()
	}
	args = append(args, "--volumes")
	return s.RemoveVolumes(func() error { return s.run(nil, args...) })
}

// MarkStopped records that the stack's containers have been stopped, clearing
// Held and LastUsedAt while keeping volumes, ports, and InitializedAt. An
// unregistered stack stays unregistered.
func (s *Stack) MarkStopped() error {
	updated, err := s.registry.MarkStopped(s.Workspace.Project())
	if err != nil {
		return err
	}
	if updated != nil {
		s.Entry = *updated
	}
	return nil
}

// RemoveVolumes runs remove, a compose call that deletes the stack's
// volumes, under the stack's lock, so it never removes them while another
// aviary is running init. Once remove succeeds the stack is recorded as
// uninitialized, so init runs again next time. A stack that is not registered
// stays unregistered.
func (s *Stack) RemoveVolumes(remove func() error) error {
	unlock, err := s.registry.LockProject(s.Workspace.Project())
	if err != nil {
		return err
	}
	defer unlock()
	if err := remove(); err != nil {
		return err
	}
	if err := s.MarkStopped(); err != nil {
		return err
	}
	if err := s.reload(); err != nil {
		return err
	}
	if s.Entry.Project == "" {
		return nil
	}
	return s.markInitialized(false)
}

// reload rereads the stack's registry entry, which another aviary process
// may have changed since Load. An unregistered stack keeps its zero entry.
func (s *Stack) reload() error {
	entry, ok, err := s.registry.Get(s.Workspace.Project())
	if err != nil {
		return err
	}
	if ok {
		s.Entry = entry
	}
	return nil
}

// markInitialized records done in the registry and keeps the updated entry.
func (s *Stack) markInitialized(done bool) error {
	entry, err := s.registry.SetInitialized(s.Workspace.Project(), done)
	if err != nil {
		return err
	}
	s.Entry = entry
	return nil
}
