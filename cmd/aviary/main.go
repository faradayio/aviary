// Command aviary runs an isolated Docker Compose stack for each worktree.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/faradayio/aviary/internal/compose"
	"github.com/faradayio/aviary/internal/config"
	"github.com/faradayio/aviary/internal/registry"
	"github.com/faradayio/aviary/internal/stack"
	"github.com/faradayio/aviary/internal/workspace"
)

// version is the release version, set at link time with
// -ldflags "-X main.version=...".
var version = "dev"

func main() {
	err := newApp().rootCmd().Execute()
	if err == nil {
		return
	}
	// A suite's or a passthrough's own status is the answer; compose has
	// already printed whatever it had to say.
	var exit *compose.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.Code)
	}
	fmt.Fprintln(os.Stderr, "aviary:", err)
	os.Exit(1)
}

// app holds the global flags and the calls that reach outside the process,
// so tests can run commands against a temp registry without Docker.
type app struct {
	config       string
	openRegistry func() (*registry.Registry, error)
	listProjects func() ([]compose.Project, error)
	downProject  func(name string) error
	stopProject  func(name string) error
	runCompose   func(c compose.Stack, args ...string) error
	runHost      func(dir string, extraEnv map[string]string, command []string) error
	spawnReaper  func(project string)
	vaultToken   func(cfg *config.Vault) (string, error)
	branch       func(root string) string
	now          func() time.Time
	sleep        func(time.Duration)
	maxSleep     time.Duration
}

func newApp() *app {
	return &app{
		openRegistry: registry.Open,
		listProjects: compose.ListProjects,
		downProject:  compose.DownProject,
		stopProject:  compose.StopProject,
		runCompose:   compose.Stack.Run,
		runHost:      compose.RunHost,
		spawnReaper:  spawnDetachedReaper,
		branch:       workspace.Branch,
		now:          time.Now,
		sleep:        time.Sleep,
		// Poll in short slices so down, rm, up, or a deleted state dir lets
		// the reaper exit within seconds instead of sleeping the full TTL.
		maxSleep: 5 * time.Second,
	}
}

func (a *app) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *app) sleepFor(d time.Duration) {
	if a.maxSleep > 0 && d > a.maxSleep {
		d = a.maxSleep
	}
	if a.sleep != nil {
		a.sleep(d)
		return
	}
	time.Sleep(d)
}

func spawnDetachedReaper(project string) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "aviary: idle reaper: %v\n", err)
		return
	}
	cmd := exec.Command(exe, "__reap-idle", project)
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "aviary: idle reaper: %v\n", err)
		return
	}
	_ = cmd.Process.Release()
}

func (a *app) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "aviary",
		Short:         "An isolated Docker Compose stack for every worktree",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Parse aviary's own flags before the command name, so that
		// passthrough commands, which hand every later argument to compose,
		// still see --config.
		TraverseChildren: true,
		// Traversal hands an unknown command name to the root as an
		// argument instead of failing, so the root rejects it itself.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
			}
			return cmd.Help()
		},
	}
	root.PersistentFlags().StringVar(&a.config, "config", "",
		"directory holding aviary.yml when the worktree has none (default $AVIARY_CONFIG)")
	root.AddCommand(
		a.upCmd(),
		a.downCmd(),
		a.testCmd(),
		a.initCmd(),
		a.pullCmd(),
		a.runCmd(),
		a.passthrough("exec <service> <command...>", "Run a command in a running container", inspectsOnly, "exec"),
		a.passthrough("ps", "List this stack's containers", inspectsOnly, "ps"),
		a.passthrough("logs [service...]", "Show container logs", inspectsOnly, "logs"),
		a.passthrough("build [service...]", "Build the stack's images", startsContainers, "build"),
		a.composeCmd(),
		a.envCmd(),
		a.lsCmd(),
		a.gcCmd(),
		a.rmCmd(),
		a.reapIdleCmd(),
	)
	return root
}

// configDir is the external definition directory: --config, else
// AVIARY_CONFIG, else empty.
func (a *app) configDir() string {
	if a.config != "" {
		return a.config
	}
	return os.Getenv("AVIARY_CONFIG")
}

// How commands load their stack, by what they do with it.
var (
	// startsContainers enforces require_env and allocates host ports.
	startsContainers = stack.Options{}
	// inspectsOnly is for commands that only look at, stop or exec into a
	// stack's existing containers: they create no container to hand
	// variables to and must not register a stack that was never started.
	inspectsOnly = stack.Options{SkipRequireEnv: true, NoAllocate: true}
)

// loadStack loads the stack for the current directory against the app's
// registry.
func (a *app) loadStack(opts stack.Options) (*stack.Stack, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if opts.Registry, err = a.openRegistry(); err != nil {
		return nil, err
	}
	if a.now != nil {
		opts.Registry.Now = a.now
	}
	opts.Runner = a.runCompose
	opts.HostRunner = a.runHost
	opts.SpawnReaper = a.spawnReaper
	opts.VaultToken = a.vaultToken
	return stack.Load(cwd, a.configDir(), opts)
}

func (a *app) upCmd() *cobra.Command {
	cmd := a.passthrough("up [--build] [service...]", "Start the stack, or the named services, and wait until healthy", startsContainers, "up", "--detach", "--wait")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		s, err := a.loadStack(startsContainers)
		if err != nil {
			return err
		}
		return s.Up(args)
	}
	return cmd
}

// pullCmd pulls images across every profile without registering the stack or
// checking require_env. For an unregistered stack, s.Entry.Ports is empty, so
// compose files that guard port mappings with `${AVIARY_PORT_X:?...}` would
// refuse to parse; fill any unallocated configured port with its BasePort
// placeholder for interpolation only.
func (a *app) pullCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pull [service...]",
		Short: "Pull the stack's images across every profile",
		Long: `Pull the stack's images across every profile (or for the named services).
When gcloud Application Default Credentials are available on the host,
CLOUDSDK_AUTH_ACCESS_TOKEN is set from them so Docker's gcloud credential
helper works unattended without an interactive gcloud reauthentication prompt.`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
				return cmd.Help()
			}
			s, err := a.loadStack(inspectsOnly)
			if err != nil {
				return err
			}
			c := s.Compose()
			c.Profiles = []string{"*"}
			base := registry.BasePort(s.Workspace.Project())
			for i, name := range s.Config.Ports {
				key := "AVIARY_PORT_" + strings.ToUpper(name)
				if c.Env[key] == "" {
					c.Env[key] = fmt.Sprint(base + i)
				}
			}
			if os.Getenv("CLOUDSDK_AUTH_ACCESS_TOKEN") == "" {
				if tok := gcloudADCToken(); tok != "" {
					c.Env["CLOUDSDK_AUTH_ACCESS_TOKEN"] = tok
				}
			}
			return a.runCompose(c, append([]string{"pull", "--quiet"}, args...)...)
		},
	}
}

// gcloudADCToken prints an access token from gcloud's Application Default
// Credentials, or returns empty when there are none, without starting gcloud
// on machines that never ran `gcloud auth application-default login`.
func gcloudADCToken() string {
	configDir := os.Getenv("CLOUDSDK_CONFIG")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configDir = filepath.Join(home, ".config", "gcloud")
	}
	if _, err := os.Stat(filepath.Join(configDir, "application_default_credentials.json")); err != nil {
		return ""
	}
	out, err := exec.Command("gcloud", "auth", "application-default", "print-access-token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (a *app) runCmd() *cobra.Command {
	cmd := a.passthrough("run <service> [command...]", "Run a one-off command in a fresh container", startsContainers, "run", "--rm")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		s, err := a.loadStack(startsContainers)
		if err != nil {
			return err
		}
		return s.RunOneOff(args)
	}
	return cmd
}

// passthrough makes a command that forwards its arguments to one compose
// subcommand, after sub and its defaults, with the stack's project, files,
// profiles and ports applied. An empty sub forwards the arguments as the
// whole compose command line.
func (a *app) passthrough(use, short string, opts stack.Options, sub ...string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Long: short + `.

Every argument is passed to docker compose unchanged, so aviary flags such as
--config go before the command name.`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.loadStack(opts)
			if err != nil {
				return err
			}
			if !opts.SkipRequireEnv {
				s.WarnDockerDesktop()
			}
			return a.runCompose(s.Compose(), slices.Concat(sub, args)...)
		},
	}
}

// composeCmd forwards a whole compose command line. A subcommand that only
// inspects or stops the stack loads it like the other inspecting commands,
// and a `down` that removes volumes marks the stack uninitialized, as
// `aviary down --volumes` does.
func (a *app) composeCmd() *cobra.Command {
	cmd := a.passthrough("compose <args...>", "Run any docker compose subcommand against this stack", startsContainers)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		sub, rest := composeSubcommand(args)
		opts := startsContainers
		if inspectingSubcommands[sub] {
			opts = inspectsOnly
		}
		s, err := a.loadStack(opts)
		if err != nil {
			return err
		}
		run := func() error { return a.runCompose(s.Compose(), args...) }
		if isDryRunOrHelp(args) {
			return run()
		}
		switch sub {
		case "up":
			return s.ComposeHold(args)
		case "run":
			return s.ComposeUse(args)
		case "down":
			if !targetsWholeStack(rest) {
				return run()
			}
			if removesVolumes(rest) {
				return s.RemoveVolumes(run)
			}
			if err := run(); err != nil {
				return err
			}
			return s.MarkStopped()
		case "stop":
			if err := run(); err != nil {
				return err
			}
			if targetsWholeStack(rest) {
				return s.MarkStopped()
			}
			return nil
		default:
			if !opts.SkipRequireEnv {
				s.WarnDockerDesktop()
			}
			return run()
		}
	}
	return cmd
}

// inspectingSubcommands are the compose subcommands that create no
// container: they look at, stop, remove or exec into the stack's existing
// ones.
var inspectingSubcommands = map[string]bool{
	"attach": true, "config": true, "cp": true, "down": true, "events": true,
	"exec": true, "images": true, "kill": true, "logs": true, "ls": true,
	"pause": true, "port": true, "ps": true, "rm": true, "stats": true,
	"stop": true, "top": true, "unpause": true, "version": true,
	"volumes": true, "wait": true,
}

// composeValueFlags are compose's global flags that take a separate value.
var composeValueFlags = map[string]bool{
	"--ansi": true, "--env-file": true, "-f": true, "--file": true,
	"--parallel": true, "--profile": true, "--progress": true,
	"-p": true, "--project-name": true, "--project-directory": true,
}

// composeSubcommand returns the compose subcommand in args, skipping the
// global flags before it, and the arguments after it.
func composeSubcommand(args []string) (sub string, rest []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			return arg, args[i+1:]
		}
		if composeValueFlags[arg] {
			i++
		}
	}
	return "", nil
}

// removesVolumes reports whether down's arguments ask it to remove volumes.
// Compose accepts -v and --volumes with an optional bool (=true, =false), and
// the last one wins. Arguments after -- are service names.
func removesVolumes(args []string) bool {
	remove := false
	for _, arg := range args {
		if arg == "--" {
			return remove
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if name != "-v" && name != "--volumes" {
			continue
		}
		if !hasValue {
			remove = true
			continue
		}
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			continue
		}
		remove = parsed
	}
	return remove
}

func isDryRunOrHelp(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--dry-run" || arg == "--dry-run=true" || arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

// commandValueFlags are flags whose following argument is a value, not a
// service name. `--rmi local` is the whole stack; `down postgres` is not.
var commandValueFlags = map[string]bool{
	"-t": true, "--timeout": true,
	"--rmi": true,
}

// targetsWholeStack reports whether args name no service, so the command
// applies to every container in the stack.
func targetsWholeStack(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return len(args[i+1:]) == 0
		}
		name, _, hasValue := strings.Cut(arg, "=")
		if strings.HasPrefix(name, "-") {
			if !hasValue && commandValueFlags[name] {
				i++
			}
			continue
		}
		return false
	}
	return true
}

func (a *app) downCmd() *cobra.Command {
	var volumes bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop this stack",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.loadStack(inspectsOnly)
			if err != nil {
				return err
			}
			return s.Down(volumes)
		},
	}
	cmd.Flags().BoolVarP(&volumes, "volumes", "v", false, "also remove volumes, so init runs again on the next test")
	return cmd
}

func (a *app) testCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test [suite [-- args...]]",
		Short: "Run a named test suite, starting what it needs first",
		Long: `Run a named test suite from aviary.yml. It first prints whether init will
run; the first init in a worktree can take several minutes. The prepare steps
run on the host unless the suite sets prepare: false, the init steps run if
the suite wants them and they have not run yet, the services the suite needs
are started and waited on, and the suite's command runs in a fresh container
(or on the host for host: true suites) with any extra args appended. The exit
status is the suite's own.

With no suite, list the suites aviary.yml defines.`,
		// Arguments after the suite name go to the suite's command, so cobra
		// parses no flags here, including its own help flag.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
				return cmd.Help()
			}
			// Listing suites starts nothing, so it neither needs require_env
			// nor registers the stack.
			opts := startsContainers
			if len(args) == 0 {
				opts = inspectsOnly
			}
			s, err := a.loadStack(opts)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				names := s.SuiteNames()
				if len(names) == 0 {
					return errors.New("aviary.yml defines no suites")
				}
				for _, name := range names {
					fmt.Fprintln(cmd.OutOrStdout(), name)
				}
				return nil
			}
			return s.Test(args[0], args[1:])
		},
	}
}

func (a *app) initCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Run the stack's init steps if they have not run yet",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.loadStack(startsContainers)
			if err != nil {
				return err
			}
			return s.Init(force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "run the init steps even if they already ran")
	return cmd
}

func (a *app) envCmd() *cobra.Command {
	var export, asJSON bool
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Print this stack's project name, root and host ports",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// env hands out the ports, so it allocates them, but it starts
			// nothing and so does not need require_env.
			s, err := a.loadStack(stack.Options{SkipRequireEnv: true})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return printJSON(out, s.Env())
			}
			lines := s.EnvLines()
			if export {
				lines = s.ExportLines()
			}
			for _, line := range lines {
				fmt.Fprintln(out, line)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&export, "export", false, "print export lines with quoted values, for eval in a shell")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as a JSON object")
	cmd.MarkFlagsMutuallyExclusive("export", "json")
	return cmd
}

// stackRow is one stack in `aviary ls`. Its JSON keys are part of aviary's
// interface for scripts and agents.
type stackRow struct {
	Project string `json:"project"`
	// Status is compose's status for the project, "down" when compose has
	// no containers for it, or "unknown" when compose could not be asked.
	Status          string `json:"status"`
	Root            string `json:"root"`
	Branch          string `json:"branch"`
	WorktreeMissing bool   `json:"worktree_missing"`
	Initialized     bool   `json:"initialized"`
	Held            bool   `json:"held"`
	// CreatedAt, InitializedAt and LastUsedAt are null when unset.
	CreatedAt     *time.Time     `json:"created_at"`
	InitializedAt *time.Time     `json:"initialized_at"`
	LastUsedAt    *time.Time     `json:"last_used_at"`
	Ports         map[string]int `json:"ports"`
	// Config is "in-tree" or the external definition directory.
	Config string `json:"config"`
}

func (a *app) lsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List every aviary stack on this machine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := a.openRegistry()
			if err != nil {
				return err
			}
			entries, err := reg.List()
			if err != nil {
				return err
			}
			// Docker being down must not hide the registry, which is what
			// someone cleaning up needs to see.
			projects, err := a.listProjects()
			rows := a.rows(entries, projects)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "aviary: docker compose ls failed: %v; statuses unknown\n", err)
				for i := range rows {
					rows[i].Status = "unknown"
				}
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), rows)
			}
			return writeTable(cmd.OutOrStdout(), rows)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as a JSON array")
	return cmd
}

func (a *app) rows(entries []registry.Entry, projects []compose.Project) []stackRow {
	status := map[string]string{}
	for _, p := range projects {
		status[p.Name] = p.Status
	}
	rows := make([]stackRow, 0, len(entries))
	for _, e := range entries {
		row := stackRow{
			Project:       e.Project,
			Status:        status[e.Project],
			Root:          e.Root,
			Initialized:   e.InitializedAt != nil,
			Held:          e.Held,
			InitializedAt: e.InitializedAt,
			LastUsedAt:    e.LastUsedAt,
			Ports:         e.Ports,
			Config:        e.Config,
		}
		if !e.CreatedAt.IsZero() {
			row.CreatedAt = &e.CreatedAt
		}
		if row.Status == "" {
			row.Status = "down"
		}
		if row.Ports == nil {
			row.Ports = map[string]int{}
		}
		if row.Config == "" {
			row.Config = "in-tree"
		}
		if rootMissing(e.Root) {
			row.WorktreeMissing = true
		} else {
			row.Branch = a.branch(e.Root)
		}
		rows = append(rows, row)
	}
	return rows
}

func writeTable(out io.Writer, rows []stackRow) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PROJECT\tSTATUS\tBRANCH\tWORKTREE\tCONFIG\tPORTS")
	for _, row := range rows {
		st := row.Status
		if row.WorktreeMissing {
			st += " (worktree missing)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			row.Project, st, orDash(row.Branch), row.Root, row.Config, formatPorts(row.Ports))
	}
	return w.Flush()
}

func (a *app) gcCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Remove stacks whose worktree is gone and stop idle test stacks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := a.openRegistry()
			if err != nil {
				return err
			}
			entries, err := reg.List()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			now := a.clock()
			// One stack that cannot be removed or stopped must not keep the rest.
			var errs []error
			for _, e := range entries {
				switch worktreeState(e.Root) {
				case worktreePresent:
					if !e.IsIdleAt(now) {
						continue
					}
					useLock, err := reg.TryLockIdle(e.Project)
					if err != nil {
						errs = append(errs, err)
						continue
					}
					if useLock == nil {
						continue
					}
					fresh, ok, err := reg.Get(e.Project)
					if err != nil {
						_ = useLock.Unlock()
						errs = append(errs, err)
						continue
					}
					if !ok || !fresh.IsIdleAt(now) {
						_ = useLock.Unlock()
						continue
					}
					idleAge := fresh.IdleFor(now).Round(time.Second)
					if dryRun {
						_ = useLock.Unlock()
						fmt.Fprintf(out, "%s: idle for %s; would stop\n", fresh.Project, idleAge)
						continue
					}
					stopErr := a.stopIdle(reg, fresh.Project)
					_ = useLock.Unlock()
					if stopErr != nil {
						errs = append(errs, stopErr)
						continue
					}
					fmt.Fprintf(out, "%s: idle for %s; stopped\n", fresh.Project, idleAge)
					continue
				case worktreeUnreachable:
					fmt.Fprintf(cmd.ErrOrStderr(), "aviary: %s: neither worktree %s nor its parent directory exists; skipped in case it is on a disk that is not mounted (aviary rm --project %s removes it)\n", e.Project, e.Root, e.Project)
					continue
				}
				if dryRun {
					fmt.Fprintf(out, "%s: worktree %s is gone; would remove\n", e.Project, e.Root)
					continue
				}
				if err := a.remove(reg, e.Project); err != nil {
					errs = append(errs, err)
					continue
				}
				fmt.Fprintf(out, "%s: worktree %s is gone; removed\n", e.Project, e.Root)
			}
			return errors.Join(errs...)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be removed or stopped without changing anything")
	return cmd
}

func (a *app) reapIdleCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__reap-idle <project>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.reapIdleProject(args[0])
		},
	}
}

func (a *app) reapIdleProject(project string) error {
	reg, err := a.openRegistry()
	if err != nil {
		return err
	}
	reaperLock, err := reg.TryClaimReaper(project)
	if err != nil || reaperLock == nil {
		return err
	}
	for {
		useLock, err := reg.TryLockIdle(project)
		if err != nil {
			_ = reaperLock.Unlock()
			return err
		}
		if useLock == nil {
			if err := reg.WaitUseIdle(project); err != nil {
				_ = reaperLock.Unlock()
				return err
			}
			continue
		}
		entry, ok, err := reg.Get(project)
		if err != nil || !ok || !entry.IsWarm() {
			_ = reaperLock.Unlock()
			_ = useLock.Unlock()
			return err
		}
		remaining := entry.IdleDeadline().Sub(a.clock())
		if remaining > 0 {
			_ = useLock.Unlock()
			a.sleepFor(remaining)
			continue
		}
		stopErr := a.stopIdle(reg, project)
		_ = reaperLock.Unlock()
		_ = useLock.Unlock()
		return stopErr
	}
}

func (a *app) rmCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "rm",
		Short: "Remove a stack with its volumes and release its ports",
		Long: `Remove a stack's containers, networks and volumes, and release its host ports.
Without --project, the stack is this worktree's. A later command that starts
the stack, or aviary env, registers it again; allocation starts again at the
block derived from the project name, so it gets the same ports back unless they
have been taken since.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := project
			if name == "" {
				// Only the project name is needed, so an aviary.yml that
				// no longer parses does not stop its stack being removed.
				cwd, err := os.Getwd()
				if err != nil {
					return err
				}
				ws, err := workspace.Detect(cwd, a.configDir())
				if err != nil {
					return err
				}
				name = ws.Project()
			}
			reg, err := a.openRegistry()
			if err != nil {
				return err
			}
			_, ok, err := reg.Get(name)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("no stack %q in the registry; see aviary ls", name)
			}
			return a.remove(reg, name)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "remove this project instead of the current worktree's, even if its worktree is gone")
	return cmd
}

// remove takes a project down by name and then forgets it. The error from
// compose is wrapped as text so the CLI reports which project failed instead
// of exiting with compose's bare status.
func (a *app) remove(reg *registry.Registry, project string) error {
	if err := a.downProject(project); err != nil {
		return fmt.Errorf("removing %s: %v", project, err)
	}
	return reg.Release(project)
}

// stopIdle stops an idle warm stack's containers while keeping its volumes,
// ports, and InitializedAt.
func (a *app) stopIdle(reg *registry.Registry, project string) error {
	if err := a.stopProject(project); err != nil {
		return fmt.Errorf("stopping %s: %v", project, err)
	}
	_, err := reg.MarkStopped(project)
	return err
}

type rootState int

const (
	worktreePresent rootState = iota
	// worktreeMissing is a deleted worktree: the root is gone but the
	// directory that held it is still there.
	worktreeMissing
	// worktreeUnreachable is a root whose parent directory is gone too,
	// as when it is on a disk or share that is not mounted.
	worktreeUnreachable
)

// worktreeState reports whether a stack's worktree has been deleted. Any
// stat failure other than not-existing counts as present, so gc never
// removes a stack's volumes over a permissions problem.
func worktreeState(root string) rootState {
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		return worktreePresent
	}
	if info, err := os.Stat(filepath.Dir(root)); err == nil && info.IsDir() {
		return worktreeMissing
	}
	return worktreeUnreachable
}

// rootMissing reports whether a stack's worktree has been deleted.
func rootMissing(root string) bool {
	return worktreeState(root) == worktreeMissing
}

func printJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func formatPorts(ports map[string]int) string {
	if len(ports) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(ports))
	for _, name := range slices.Sorted(maps.Keys(ports)) {
		parts = append(parts, fmt.Sprintf("%s=%d", name, ports[name]))
	}
	return strings.Join(parts, " ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
