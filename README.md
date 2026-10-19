# aviary

An isolated Docker Compose stack for every git worktree.

When several agents or people work on one repository in parallel, each
worktree needs its own running copy of the application: its own containers,
its own database, its own host ports, running the code from that worktree.
Plain `docker compose` assumes one checkout per machine. aviary gives each
worktree a compose project name and a set of host ports of its own, runs
named test suites against it, tracks every stack on the machine, and cleans
up after worktrees that are gone.

Everything else is plain compose. aviary adds no runtime, daemon or compose
dialect, writes nothing into the worktree, and your compose files keep working
without it.

## Install

aviary needs Docker with Compose v2.

```sh
go install github.com/faradayio/aviary/cmd/aviary@latest
```

## Set up a repository

Put an `aviary.yml` next to your compose files:

```yaml
compose:
  files: [compose.yaml, compose.dev.yaml]   # relative to aviary.yml
ports: [postgres, api]
```

`examples/minimal` is a complete working stack: Postgres, Redis, an init step
and two suites. Run `aviary test db` from that directory to try it.

### Ports

Each name under `ports` gets one host port per worktree, passed to compose as
`AVIARY_PORT_<NAME>` (the name upper-cased). Ports start at a block between
20000 and 59999 derived from the worktree's project name, so a worktree at the
same path gets the same ports on every machine; they move only when another
stack or process already holds them, and once allocated they are kept.
Use the variable where the compose file publishes a port, with the usual port
as the default so the file still works without aviary:

```yaml
services:
  postgres:
    ports:
      - "${AVIARY_PORT_POSTGRES:-5432}:5432"
```

Port names are lower-case letters, digits and underscores, starting with a
letter. A worktree keeps its ports until its stack is removed with `aviary rm`
or `aviary gc`.

### Required environment

```yaml
require_env: [VAULT_TOKEN]
```

Commands that start or run containers (`up`, `test`, `init`, `run`, `exec`,
`build`, `compose`) refuse to start, listing every missing name, when any of
these variables is unset or empty. The host environment passes through to
compose, so the compose files can interpolate them.

### Definitions outside the worktree

aviary finds the stack definition in one of two places, in this order:

1. **In-tree.** The nearest `aviary.yml` in the current directory or a parent,
   within the current checkout. That directory is the worktree root.
2. **External.** When there is no in-tree file, the directory named by
   `--config <dir>` or, failing that, `AVIARY_CONFIG`. The worktree root is
   then `git rev-parse --show-toplevel` of the current directory.

An external definition lets every existing worktree of a repository, on any
branch, run a stack before the repository carries an `aviary.yml` of its own:

```sh
export AVIARY_CONFIG=~/stacks/myrepo
cd ~/src/myrepo-worktrees/feature-x
aviary up
```

Either way, compose file paths in `aviary.yml` are relative to the definition
directory, and compose's project directory is the worktree root, so bind
mounts and `env_file` paths resolve against the worktree.

`--config` is a flag of `aviary` itself, so it goes before the command name:
`aviary --config ~/stacks/myrepo up`.

## Test suites and init

Name the test runs a repository offers, and the one-time setup they depend
on:

```yaml
init:
  - service: migrate
    command: [rake, db:reset]
  - service: seed
    command: [bin/seed]
suites:
  api:
    service: api
    needs: [postgres, redis]
    command: [npm, test]
  api-empty-db:
    service: api
    needs: [postgres]
    command: [npm, run, test:migrations]
    init: false
```

`aviary test api`:

1. runs the init steps, each as `docker compose run --rm <service> <command>`,
   if init has not run for this stack yet;
2. starts `postgres` and `redis` and waits until they are healthy;
3. runs `npm test` in a fresh `api` container.

Arguments after `--` are appended to the suite's command, so
`aviary test api -- --grep users` reaches the test runner. aviary exits with
the suite's own status. A suite with `init: false` skips step 1.

Init runs once per stack. A failing step stops the sequence, names the step,
and leaves the stack uninitialized with the partial data kept for inspection.
`aviary down --volumes` and `aviary rm` discard the data so init runs again
next time; `aviary init --force` reruns the steps against the current data.
aviary records that init has run rather than checking the volumes, so removing
them any other way, such as `aviary compose down -v`, leaves the stack marked
initialized until `aviary init --force`. Checking that the volumes exist
arrives with seeded snapshots.

`aviary test` with no suite lists the suites, and `aviary test --help` or
`aviary test -h` prints the command's help. Every argument after a suite name
is passed to the suite, so `aviary test api --help` reaches the test runner.

### Profiles

Compose profiles keep services that only some runs need, such as a browser
test runner, in the same definition without starting them on every `up`:

```yaml
compose:
  files: [compose.yaml]
  profiles: [dev]                 # applied to every compose call
suites:
  acceptance:
    service: acceptance
    profiles: [acceptance]        # added for this suite only
    needs: [web, api]
    command: [pnpm, test]
```

Stack-wide profiles are passed as `--profile` to every compose call. A
suite's profiles are added to both of its calls: the `up` of its needs and the
`run` of its command.

## Seeded snapshots (coming)

Caching seeded volumes and restoring them into new worktrees instead of
running init is planned. `aviary.yml` already accepts the `snapshot` and
`after_restore` keys so definitions written now keep working, but aviary does
not act on them yet: init always runs its steps.

## Commands

Run from anywhere inside a worktree.

- `aviary up [--build] [service...]` starts the stack, or the named services,
  detached, and waits until they are healthy.
- `aviary down` stops it and keeps its volumes; `aviary down --volumes` removes
  them too.
- `aviary test <suite> [-- args...]` runs a suite (see above); with no suite it
  lists them.
- `aviary init [--force]` runs the init steps by hand.
- `aviary run <service> [command...]` runs a one-off command in a fresh
  container and exits with its status.
- `aviary exec <service> <command...>` runs a command in a running container.
- `aviary ps`, `aviary logs` and `aviary build` pass through to the compose
  subcommand of the same name; `aviary compose <args...>` runs any compose
  subcommand. The stack's project, directory, files, profiles and ports are
  applied, and every argument goes to compose unchanged.
- `aviary env` prints the stack's variables as `KEY=value` lines;
  `eval "$(aviary env --export)"` puts them in your shell, and `--json` prints
  them as an object.
- `aviary ls [--json]` lists every aviary stack on the machine.
- `aviary gc [--dry-run]` removes stacks, volumes included, whose worktree
  directory no longer exists, and releases their ports.
- `aviary rm [--project <name>]` removes this worktree's stack, or the named
  one, with its volumes, and releases its ports. A later command that starts
  the stack, or `aviary env`, registers it again; allocation starts again at
  the block derived from the project name (see [Ports](#ports)), so it gets the
  same ports back unless they have been taken since.

No command prompts. aviary's own errors go to stderr as `aviary: <message>`
with exit status 1.

### JSON output

`aviary env --json` prints one object of string values:

| Key                     | Value                                        |
| ----------------------- | -------------------------------------------- |
| `COMPOSE_PROJECT_NAME`  | the compose project name                     |
| `AVIARY_PROJECT`        | the same project name                        |
| `AVIARY_SLUG`           | the project name without the `aviary-` prefix |
| `AVIARY_ROOT`           | the worktree root                            |
| `AVIARY_PORT_<NAME>`    | the host port for each name in `ports`       |

`aviary ls --json` prints an array with one object per stack:

| Key                | Type    | Value                                                                    |
| ------------------ | ------- | ------------------------------------------------------------------------ |
| `project`          | string  | compose project name                                                     |
| `status`           | string  | compose's status, such as `running(3)` or `exited(2)`, `down` when the stack has no containers, or `unknown` when `docker compose ls` failed |
| `root`             | string  | worktree root                                                            |
| `branch`           | string  | current git branch of the root, empty when unknown or detached           |
| `worktree_missing` | boolean | the root no longer exists; `aviary gc` will remove the stack             |
| `initialized`      | boolean | init has run since the stack was created or since aviary last removed its volumes (`down --volumes` or `rm`) |
| `ports`            | object  | port name to host port                                                   |
| `config`           | string  | `in-tree`, or the external definition directory                          |

These keys are stable; new keys may be added.

## How isolation works

The compose project name is `aviary-<slug>`, where the slug is the worktree
root's directory name (lower-cased, other characters replaced by `-`, at most
30 characters) followed by `-` and the first six hex digits of the SHA-1 of the
root's full path. Two worktrees of one repository therefore never share
containers, networks or named volumes, even when their directories have the
same name.

Every compose call is
`docker compose --project-name <project> --project-directory <root> --file ...`,
with aviary's variables added to the subprocess environment. Nothing is
generated into the worktree, so concurrent aviary commands in one worktree
never race over files.

Host ports are recorded in `$XDG_STATE_HOME/aviary/registry.json` (default
`~/.local/state`), written under a file lock. Before handing out a port, aviary
skips any port another stack holds and checks that nothing else on the machine
is listening on it. If something takes the port between allocation and
`aviary up`, compose reports the conflict; `aviary rm` and then `aviary up`
allocate new ports.

A relative `XDG_STATE_HOME` is ignored. aviary refuses a registry file whose
`version` it does not read, or with an entry whose `project` is missing or
differs from its key; the error names the file and the entry to fix or delete.

## Development

```sh
go test ./...                                  # unit tests, no Docker needed
go test -tags integration ./cmd/aviary         # runs examples/minimal end to end in Docker
```

The integration test uses temporary XDG state and cache directories, so it
never touches the machine's registry, and removes its compose project even
when it fails.

## License

Apache-2.0. See [LICENSE](LICENSE).
