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
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/faradayio/aviary/internal/compose"
	"github.com/faradayio/aviary/internal/registry"
	"github.com/faradayio/aviary/internal/stack"
	"github.com/faradayio/aviary/internal/workspace"
)

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
	runCompose   func(c compose.Stack, args ...string) error
	branch       func(root string) string
}

func newApp() *app {
	return &app{
		openRegistry: registry.Open,
		listProjects: compose.ListProjects,
		downProject:  compose.DownProject,
		runCompose:   func(c compose.Stack, args ...string) error { return c.Run(args...) },
		branch:       workspace.Branch,
	}
}

func (a *app) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "aviary",
		Short:         "An isolated Docker Compose stack for every worktree",
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
		a.passthrough("up [--build] [service...]", "Start the stack, or the named services, and wait until healthy", true, "up", "--detach", "--wait"),
		a.downCmd(),
		a.testCmd(),
		a.initCmd(),
		a.passthrough("run <service> [command...]", "Run a one-off command in a fresh container", true, "run", "--rm"),
		a.passthrough("exec <service> <command...>", "Run a command in a running container", true, "exec"),
		a.passthrough("ps", "List this stack's containers", false, "ps"),
		a.passthrough("logs [service...]", "Show container logs", false, "logs"),
		a.passthrough("build [service...]", "Build the stack's images", true, "build"),
		a.passthrough("compose <args...>", "Run any docker compose subcommand against this stack", true),
		a.envCmd(),
		a.lsCmd(),
		a.gcCmd(),
		a.rmCmd(),
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

// loadStack loads the stack for the current directory. checkEnv enforces
// require_env, which only commands that start or run containers need.
func (a *app) loadStack(checkEnv bool) (*stack.Stack, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	reg, err := a.openRegistry()
	if err != nil {
		return nil, err
	}
	return stack.Load(cwd, a.configDir(), stack.Options{Registry: reg, SkipRequireEnv: !checkEnv})
}

// passthrough makes a command that forwards its arguments to one compose
// subcommand, after sub and its defaults, with the stack's project, files,
// profiles and ports applied. An empty sub forwards the arguments as the
// whole compose command line.
func (a *app) passthrough(use, short string, checkEnv bool, sub ...string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Long: short + `.

Every argument is passed to docker compose unchanged, so aviary flags such as
--config go before the command name.`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.loadStack(checkEnv)
			if err != nil {
				return err
			}
			return a.runCompose(s.Compose(), slices.Concat(sub, args)...)
		},
	}
}

func (a *app) downCmd() *cobra.Command {
	var volumes bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop this stack",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.loadStack(false)
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
		Long: `Run a named test suite from aviary.yml. The stack's init steps run first if
the suite wants them and they have not run yet, the services the suite needs
are started and waited on, and the suite's command runs in a fresh container
with any extra args appended. The exit status is the suite's own.

With no suite, list the suites aviary.yml defines.`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Listing suites starts nothing, so it does not need require_env.
			s, err := a.loadStack(len(args) > 0)
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
			s, err := a.loadStack(true)
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
			s, err := a.loadStack(false)
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
	// Status is compose's status for the project, or "down" when compose
	// has no containers for it.
	Status          string         `json:"status"`
	Root            string         `json:"root"`
	Branch          string         `json:"branch"`
	WorktreeMissing bool           `json:"worktree_missing"`
	Initialized     bool           `json:"initialized"`
	Ports           map[string]int `json:"ports"`
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
			projects, err := a.listProjects()
			if err != nil {
				return err
			}
			rows := a.rows(entries, projects)
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
			Project:     e.Project,
			Status:      status[e.Project],
			Root:        e.Root,
			Initialized: e.InitializedAt != nil,
			Ports:       e.Ports,
			Config:      e.Config,
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
		Short: "Remove stacks whose worktree no longer exists, including their volumes",
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
			for _, e := range entries {
				if !rootMissing(e.Root) {
					continue
				}
				if dryRun {
					fmt.Fprintf(out, "%s: worktree %s is gone; would remove\n", e.Project, e.Root)
					continue
				}
				if err := a.remove(reg, e.Project); err != nil {
					return err
				}
				fmt.Fprintf(out, "%s: worktree %s is gone; removed\n", e.Project, e.Root)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be removed without removing it")
	return cmd
}

func (a *app) rmCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "rm",
		Short: "Remove a stack with its volumes and release its ports",
		Long: `Remove a stack's containers, networks and volumes, and release its host ports.
Without --project, the stack is this worktree's. A later command in the
worktree creates the stack again with newly allocated ports.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if project == "" {
				s, err := a.loadStack(false)
				if err != nil {
					return err
				}
				if err := s.Remove(); err != nil {
					return fmt.Errorf("removing %s: %v", s.Workspace.Project(), err)
				}
				return nil
			}
			reg, err := a.openRegistry()
			if err != nil {
				return err
			}
			entries, err := reg.List()
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(entries, func(e registry.Entry) bool { return e.Project == project }) {
				return fmt.Errorf("no stack %q in the registry; see aviary ls", project)
			}
			return a.remove(reg, project)
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

// rootMissing reports whether a stack's worktree has been deleted. Any other
// stat failure counts as present, so gc never removes a stack's volumes over
// a permissions problem.
func rootMissing(root string) bool {
	_, err := os.Stat(root)
	return errors.Is(err, fs.ErrNotExist)
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
