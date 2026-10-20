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

To try it, [install](#install) aviary, then run the example stack from a clone
of this repository:

```sh
cd examples/minimal
aviary test db      # starts Postgres, runs init, prints 2
aviary rm           # removes the stack and its volumes
```

## Why aviary

Development at Faraday moved from one checkout per person to many worktrees
per machine, most of them driven by coding agents. Each worktree needs a stack
of its own so that agents can run test suites, model training and integration
tests side by side without one worktree's database or ports clobbering
another's. Our previous tool, [cage](https://github.com/faradayio/cage),
assumes one checkout per machine, so a second worktree either collides with
the first or has to be started by hand with different settings.

**Differences from cage.**

| cage                                                | aviary                                                                          |
| --------------------------------------------------- | ------------------------------------------------------------------------------- |
| Generates compose files from its own format         | Runs your hand-written compose files as they are; generates nothing             |
| One stack per machine, fixed host ports             | One stack per worktree, a port set per worktree                                 |
| Writes into the checkout (`pods/`, mounted sources) | Writes nothing into the worktree; sources are bind-mounted by your compose file |
| Pods, targets and overlays                          | Compose profiles and extra compose files                                        |
| Mints a Vault token per service                     | Checks that the variables in `require_env` are set and passes them through      |
| Build and deploy path for production                | Local development and testing only                                              |

cage remains the deploy tool. aviary replaces it only for running a repository
locally, so a repository can carry both. [Coming from cage](#coming-from-cage)
maps cage's verbs to aviary's.

**Why Go.** Docker and Compose are written in Go, so the compose parser is an
import away if aviary ever needs it, and the standard library covers the rest
(file locks, JSON, subprocesses, port probes). goreleaser gives a static binary
per platform and a Homebrew cask. Forking cage would have kept the compose
generation that causes the collisions, and Rust's strengths don't matter for a
tool that spends its time waiting on `docker compose`.

## Install

aviary needs Docker with Compose v2.

With Homebrew on macOS:

```sh
brew install faradayio/tap/aviary
```

With Go 1.27.1 or later (prints `dev` for `aviary --version`):

```sh
go install github.com/faradayio/aviary/cmd/aviary@latest
```

Or download `aviary_<version>_<os>_<arch>.tar.gz` (`darwin` or `linux`,
`amd64` or `arm64`) from the
[releases page](https://github.com/faradayio/aviary/releases), check it
against `checksums.txt`, and put `aviary` on your `PATH`. On macOS, clear the
browser's quarantine with `xattr -d com.apple.quarantine aviary`.

## Docker Desktop settings

On macOS, Docker Desktop runs every container in one Linux virtual machine,
and its settings decide how fast each stack runs and how many fit on the
machine. Set these once, under Settings in the Docker Desktop menu, before the
first `aviary up`. On Linux there is no VM and nothing to set.

### Preferred setup

| Setting                                                          | Choose                                   |
| ---------------------------------------------------------------- | ---------------------------------------- |
| General, Virtual Machine Manager                                 | Apple Virtualization framework           |
| General, Use Rosetta for x86_64/amd64 emulation on Apple Silicon | On                                       |
| General, file sharing implementation                             | VirtioFS                                 |
| Resources, Memory                                                | Sum of the stacks you run at once + 2 GB |
| Resources, CPUs                                                  | All but two of the machine's cores       |
| Resources, Disk image size                                       | Images + one database per worktree       |
| Shell startup files                                              | No `DOCKER_DEFAULT_PLATFORM`             |

Apply, let Docker Desktop restart, then check the two that matter most:

```sh
docker info --format '{{.MemTotal}}'   # the VM's memory, in bytes
docker run --rm --platform linux/amd64 alpine uname -m   # x86_64: emulation works
```

Keep Docker Desktop current; these options are recent and still changing.

### Why each option

- **Virtual Machine Manager: Apple Virtualization framework.** Docker VMM
  starts faster, but Docker documents that it does not support Rosetta, so
  every `linux/amd64` image runs under slow emulation. Most application
  images built by CI on amd64 runners are amd64-only.
- **Rosetta: on.** Without it, amd64 images on Apple silicon run under QEMU:
  several times slower, slow enough for suites to hit their own timeouts, and
  heavy JIT processes can crash with signal 11. The option appears only with
  the Apple Virtualization framework selected.
- **File sharing: VirtioFS.** Stacks bind-mount the worktree's sources, so
  every source read goes through file sharing. gRPC FUSE is the fallback.
- **Memory.** The one limit every stack shares. A stack of databases and
  caches costs a few hundred megabytes, a full application or heavy suite
  costs gigabytes. Measure with `docker stats`, add headroom, and stay under
  half the machine's memory so the host isn't starved. See
  [Running out of memory](#running-out-of-memory).
- **CPUs.** Emulated processes are the slow ones and gain most from cores.
- **Disk image size.** Each worktree has its own named volumes, so the disk
  holds one database per stack plus the images. `docker system df` shows the
  usage.
- **`DOCKER_DEFAULT_PLATFORM`.** Remove `DOCKER_DEFAULT_PLATFORM=linux/amd64`
  from `~/.zshrc` or `~/.bashrc`. It forces multi-arch images such as Postgres
  and Redis to run emulated too.
- **Resource Saver** can stay on. It stops the idle VM, which adds a few
  seconds to the first command after a pause.

## Set up a repository

Put an `aviary.yml` next to your compose files:

```yaml
compose:
  files: [compose.yaml, compose.dev.yaml]   # relative to aviary.yml
ports: [postgres, api]
```

[`examples/minimal`](examples/minimal) is a complete working stack: Postgres,
Redis, an init step and two suites.

### Ports

Each name under `ports` gets a host port per worktree, passed to compose as
`AVIARY_PORT_<NAME>` (upper-cased). Names are lower-case letters, digits and
underscores, starting with a letter.

Publish the variable in the compose file, with the usual port as the default
so the file still works without aviary:

```yaml
services:
  postgres:
    ports:
      - "${AVIARY_PORT_POSTGRES:-5432}:5432"
```

Ports come from 20000-32759, starting at a block derived from the worktree's
path, so a worktree at the same path gets the same ports on every machine.
aviary skips ports another stack holds or something on the machine is
listening on. A worktree keeps its ports until `aviary rm` or `aviary gc`.

### Required environment

```yaml
require_env: [VAULT_TOKEN]
```

Commands that create containers (`up`, `test`, `init`, `run`, `build`, and
`aviary compose up` or `run`) refuse to start while any of these is unset or
empty, and name every missing one. The host environment passes through to
compose, so compose files can interpolate them.

### Definitions outside the worktree

aviary looks for the stack definition in this order:

1. **In-tree.** The nearest `aviary.yml` in the current directory or a parent,
   within the current checkout. That directory is the worktree root.
2. **External.** The directory named by `--config <dir>` or, failing that,
   `AVIARY_CONFIG`. The worktree root is then `git rev-parse --show-toplevel`.

An external definition lets every existing worktree of a repository, on any
branch, run a stack before the repository carries an `aviary.yml` of its own:

```sh
export AVIARY_CONFIG=~/stacks/myrepo
cd ~/src/myrepo-worktrees/feature-x
aviary up
```

`--config` goes before the command name: `aviary --config ~/stacks/myrepo up`.

Either way, compose file paths in `aviary.yml` are relative to the definition
directory, while compose's project directory is the worktree root, so bind
mounts and `env_file` paths resolve against the worktree. To reach files kept
next to the definition, use `AVIARY_CONFIG_DIR`; `AVIARY_ROOT` is the worktree
root:

```yaml
services:
  postgres:
    volumes:
      - ${AVIARY_CONFIG_DIR}/support/init.sql:/docker-entrypoint-initdb.d/init.sql:ro
```

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
   if init has not run for this stack yet (skipped with `init: false`);
2. starts `postgres` and `redis` and waits until they are healthy;
3. runs `npm test` in a fresh `api` container.

Arguments after the suite name, with or without a `--`, are appended to its
command, so `aviary test api -- --grep users` and `aviary test api --help`
reach the test runner. aviary exits with the suite's status. `aviary test`
alone lists the suites; `aviary test -h` prints the command's help.

Init runs once per stack, so a new worktree runs it from scratch. Step times
go to stderr. A failing step stops the sequence, names the step, and leaves
the stack uninitialized with the partial data kept for inspection. To run init
again:

- `aviary down --volumes`, `aviary rm` or `aviary compose down -v` discards the
  data, and init runs on the next use;
- `aviary init --force` reruns the steps against the current data.

aviary records in its registry that init has run rather than checking the
volumes, so volumes removed any other way, such as `docker volume rm`, leave
the stack marked initialized until `aviary init --force`. Suites started at
once in one worktree share a single init run.

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

Stack-wide profiles are passed as `--profile` to every compose call, which
overrides `COMPOSE_PROFILES`. A suite's profiles are added to both of its
calls: the `up` of its needs and the `run` of its command.

## Commands

Run from anywhere inside a worktree. `aviary <command> --help` describes each.

- `aviary up [--build] [service...]` starts the stack, or the named services,
  detached, and waits until they are healthy.
- `aviary down` stops it and keeps its volumes; `--volumes` removes them too.
- `aviary test <suite> [args...]` runs a suite; with no suite it lists them.
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
- `aviary gc [--dry-run]` removes every stack whose worktree directory is gone,
  volumes included, and releases its ports (see
  [Directories deleted by hand](#directories-deleted-by-hand)).
- `aviary rm [--project <name>]` removes this worktree's stack, or the named
  one, with its volumes, and releases its ports. The next command that starts
  it usually gets the same ports back.

Commands that only look at, stop or exec into containers (`ps`, `logs`,
`exec`, `down`) skip `require_env` and do not register the stack. No command
prompts. aviary's own errors go to stderr as `aviary: <message>` with exit
status 1.

### JSON output

`aviary env --json` prints one object of string values:

| Key                    | Value                                                                                                        |
| ---------------------- | ------------------------------------------------------------------------------------------------------------ |
| `COMPOSE_PROJECT_NAME` | the compose project name                                                                                     |
| `AVIARY_PROJECT`       | the same project name                                                                                        |
| `AVIARY_SLUG`          | the worktree's slug (see [How isolation works](#how-isolation-works))                                        |
| `AVIARY_ROOT`          | the worktree root                                                                                            |
| `AVIARY_CONFIG_DIR`    | the directory holding `aviary.yml`: the worktree root in-tree, else the external definition directory        |
| `AVIARY_PORT_<NAME>`   | the host port for each name in `ports`                                                                       |

`aviary ls --json` prints an array with one object per stack:

| Key                | Type    | Value                                                                                                              |
| ------------------ | ------- | ------------------------------------------------------------------------------------------------------------------ |
| `project`          | string  | compose project name                                                                                               |
| `status`           | string  | compose's status, such as `running(3)` or `exited(2)`; `down` with no containers; `unknown` if `docker compose ls` failed |
| `root`             | string  | worktree root                                                                                                      |
| `branch`           | string  | current git branch of the root, empty when unknown or detached                                                     |
| `worktree_missing` | boolean | the root no longer exists; `aviary gc` will remove the stack                                                       |
| `initialized`      | boolean | init has run since the stack was created or aviary last removed its volumes                                        |
| `created_at`       | string  | when the stack was registered, RFC 3339, or `null` when the registry has no time                                   |
| `initialized_at`   | string  | when init last completed, RFC 3339, or `null` while `initialized` is false                                         |
| `ports`            | object  | port name to host port                                                                                             |
| `config`           | string  | `in-tree`, or the external definition directory                                                                    |

These keys are stable; new keys may be added.

## Worktree tools

aviary needs nothing from the tool that makes a worktree. A stack belongs to
a directory, and the first command that needs it creates it. Calling aviary
from a worktree tool only saves typing.

### Plain git

```sh
git worktree add ../myrepo-feature-x -b feature-x
cd ../myrepo-feature-x
aviary up                      # the first command creates the stack
```

When the work is done, remove the stack and then the worktree, or the other
way round and let `aviary gc` find the stack:

```sh
aviary rm                      # inside the worktree
git worktree remove ../myrepo-feature-x

git worktree remove ../myrepo-feature-x
aviary gc                      # anywhere
```

A script that makes and removes worktrees should let a failing aviary command
warn rather than fail, so the worktree itself is always made or removed.

### Claude Code

Claude Code makes worktrees for `claude --worktree`, for subagents with
`isolation: worktree` and for background sessions. Without hooks, aviary needs
nothing: the first `aviary up` or `aviary test` creates the stack, and
`aviary gc` removes it after Claude Code removes the worktree.

To start the stack with the worktree and remove it with the worktree, add
`WorktreeCreate` and `WorktreeRemove` hooks to Claude Code's `settings.json`:

```json
{
  "hooks": {
    "WorktreeCreate": [
      { "hooks": [{ "type": "command", "command": "$HOME/bin/aviary-worktree-create" }] }
    ],
    "WorktreeRemove": [
      { "hooks": [{ "type": "command", "command": "$HOME/bin/aviary-worktree-remove" }] }
    ]
  }
}
```

A `WorktreeCreate` hook replaces Claude Code's own `git worktree add`: it must
create the worktree and print its path as the last line of stdout, with
everything else on stderr. This one uses Claude Code's location and branch
name (`.claude/worktrees/<name>`, `worktree-<name>`) but branches from the
current `HEAD`:

```sh
#!/bin/sh
# aviary-worktree-create: make the worktree, then start its stack.
set -eu
input=$(cat)
name=$(printf '%s' "$input" | jq -r .name)
repo=$(git -C "$(printf '%s' "$input" | jq -r .cwd)" rev-parse --show-toplevel)
dir="$repo/.claude/worktrees/$name"
git -C "$repo" worktree add -b "worktree-$name" "$dir" >&2
(cd "$dir" && aviary up >&2) || echo "aviary up failed; the worktree has no stack yet" >&2
echo "$dir"
```

A `WorktreeRemove` hook runs only for worktrees the `WorktreeCreate` hook
made, and must delete the directory:

```sh
#!/bin/sh
# aviary-worktree-remove: remove the stack, then the worktree.
set -u
dir=$(jq -r .worktree_path)
(cd "$dir" && aviary rm >&2) || true
git -C "$dir" worktree remove --force "$dir"
```

- The session waits for `aviary up` to report the stack healthy. If stacks are
  slow to start or rarely needed, leave `aviary up` out of the create hook and
  keep the remove hook. Subagent worktrees go through the same hooks.
- When a subagent finishes, Claude Code keeps the worktree the hook made for it
  and does not run `WorktreeRemove`. Remove it with `git worktree remove`, then
  run `aviary gc`.
- Claude Code never deletes the branch of a worktree a hook made. Delete
  `worktree-<name>` yourself once its work is merged or abandoned.

### Directories deleted by hand

A worktree directory deleted with `rm -rf`, or by a tool that knows nothing
about aviary, leaves its stack behind: containers, volumes and ports.
`aviary ls` marks it `(worktree missing)`, and `aviary gc` removes it whenever
you get to it. git keeps listing such a worktree until `git worktree prune`.

When the directory that held the worktree is missing too, `gc` skips the stack
with a warning, in case it is on a disk that is not mounted;
`aviary rm --project <name>` removes it. A stack that fails to come down does
not stop `gc` removing the others.

## Coming from cage

The [differences from cage](#why-aviary) come down to this: aviary keeps your
compose files as they are and gives every worktree its own ports. cage's verbs,
and those of similar compose wrappers, map like this:

| Wrapper verb                   | aviary                                                                                            |
| ------------------------------ | ------------------------------------------------------------------------------------------------- |
| `up`, `start`                  | `aviary up [service...]`                                                                          |
| `stop`                         | `aviary down` removes the containers and keeps the volumes; `aviary compose stop` only stops them |
| `status`                       | `aviary ps` for this worktree's stack, `aviary ls` for every stack on the machine                 |
| `run`                          | `aviary run <service> [command...]`                                                               |
| `exec`, `shell`                | `aviary exec <service> <command...>`                                                              |
| `test`                         | `aviary test <suite>`, with the suites named in `aviary.yml`                                      |
| `logs`, `build`                | `aviary logs`, `aviary build`                                                                     |
| `pull`                         | `aviary compose pull`                                                                             |
| init, seed                     | `aviary init`; `aviary down --volumes` first to start from empty volumes                          |
| source mount, unmount          | none: the compose files bind-mount the worktree's sources, so each stack always runs its own code |
| targets, environments          | more files under `compose.files`, or [profiles](#profiles)                                        |
| print the merged configuration | `aviary compose config`                                                                           |

The development compose file mounts the sources, with paths relative to the
worktree root. An anonymous volume over a dependency directory keeps the
image's copy instead of the host's (see
[Old dependencies after pulling an image](#old-dependencies-after-pulling-an-image)):

```yaml
services:
  api:
    volumes:
      - ./src/api:/app
      - /app/node_modules
```

Anything that reached a service at a fixed host port, such as `localhost:5432`
in a database client, now takes the port from `aviary env`. Configuration that
embeds a fixed host port, such as a callback URL, does not follow the
allocated port.

## How isolation works

The compose project name is `aviary-<slug>`. The slug is the worktree root's
directory name (lower-cased, other characters replaced by `-`, at most 30
characters), then `-` and the first six hex digits of the SHA-1 of the root's
full path; a slug that already starts with `aviary-` is not prefixed again.
Two worktrees of one repository therefore never share containers, networks or
named volumes, even when their directories have the same name.

Every compose call is
`docker compose --project-name <project> --project-directory <root> --file ...`,
with aviary's variables added to the subprocess environment. Nothing is
generated into the worktree, so concurrent aviary commands in one worktree
never race over files.

Host ports and init state are recorded in `$XDG_STATE_HOME/aviary/registry.json`
(default `~/.local/state`; a relative `XDG_STATE_HOME` is ignored), written
under a file lock. See [Ports](#ports) for how ports are chosen.

## Troubleshooting

### Port already allocated

aviary checks that a port is free only when it allocates it. If something
starts listening on one of a stack's ports later, `aviary up` fails with
compose's "port is already allocated" or "address already in use".
`aviary env` lists the stack's ports, and
`lsof -nP -iTCP:<port> -sTCP:LISTEN` shows what holds one.

Stop that process, or run `aviary rm` and then `aviary up` to get new ports.
`aviary rm` deletes the stack's volumes, so its data is lost and init runs
again; free the port instead when the data matters.

A compose file that publishes a fixed host port, such as `"5432:5432"`, fails
the same way in the second worktree that starts it; see [Ports](#ports).

### Running out of memory

Every stack is a full copy of the services it runs, so several worktrees with
the whole application up cost several times one. The limit is Docker
Desktop's VM memory (see [Docker Desktop settings](#docker-desktop-settings)),
or the host's on Linux. When memory runs out, containers are killed (exit
status 137) or stop passing their health checks.
`docker stats --no-stream $(aviary ps -q)` shows what this stack uses.

Lay the definition out so that a bare `aviary up` starts only what most work
needs, usually the databases and caches, and put each heavier group of
services behind a [profile](#profiles). The same machine then holds one full
stack or several lean ones. Within a stack, start more only when needed:

- `aviary up <service>` starts a service even when it sits behind a profile.
- `aviary compose --profile <name> up --detach --wait` starts a whole profile.
- A suite starts only its `needs`, with its own profiles.

`aviary down` in worktrees not in use frees their memory and keeps their data.
A memory limit in a compose file (`deploy.resources.limits.memory`) caps one
service, so a runaway service is killed on its own instead of starving every
stack.

### Old dependencies after pulling an image

An anonymous volume over a dependency directory, such as `/app/node_modules`
under a source bind mount, outlives the container: when compose recreates the
container for a newer image, it keeps the old volume and so the old
dependencies. After `aviary compose pull` or `aviary build`, start the stack
with `aviary up --renew-anon-volumes`. `aviary run` and `aviary test` start a
new container each time, so they always see the image's copy.

### Registry file errors

aviary refuses a registry file it cannot trust, and the error names the file:

- `registry version <n> is not supported`: if `<n>` is newer than this aviary
  reads, upgrade aviary (an older one never overwrites a newer file). If it is
  `0` (the key is missing) or otherwise wrong, the file was edited by hand:
  restore `"version": 1` at the top level.
- A JSON parse error: the file was edited by hand. Fix the JSON.
- `entry "<key>" has project "<name>"`: set the entry's `project` to its key,
  or delete the entry.

Deleting an entry, or the whole file, makes aviary forget those stacks without
removing them. Their names start with `aviary-`:

- `docker compose ls --all` lists the ones that still have containers;
- `docker volume ls --filter name=aviary-` lists the ones with only volumes;
- `docker compose --project-name <project> down --volumes` removes one.

A forgotten stack whose worktree still exists is registered again by the next
command that starts it, marked uninitialized, so init runs again over the data
its volumes still hold; run `aviary down --volumes` first to start clean.

## Development

```sh
go test ./...                                  # unit tests, no Docker needed
go test -tags integration ./cmd/aviary         # runs examples/minimal end to end in Docker
```

The integration test uses temporary XDG state and cache directories, so it
never touches the machine's registry, and removes its compose project even
when it fails.

Pushing a `v*` tag runs the release workflow: GoReleaser builds the archives
and checksums, publishes the GitHub release, and updates the Homebrew cask in
`faradayio/homebrew-tap` when the `HOMEBREW_TAP_GITHUB_TOKEN` secret is set and
the tag is not a prerelease. `goreleaser release --snapshot --clean` builds the
same archives into `dist/` locally without publishing anything.

## License

Apache-2.0. See [LICENSE](LICENSE).
