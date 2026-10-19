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

With Homebrew on macOS:

```sh
brew install faradayio/tap/aviary
```

With Go 1.27.1 or later (prints `dev` for `aviary --version`):

```sh
go install github.com/faradayio/aviary/cmd/aviary@latest
```

Or download the archive for your platform from the
[releases page](https://github.com/faradayio/aviary/releases), named
`aviary_<version>_<os>_<arch>.tar.gz` for `darwin` or `linux` on `amd64` or
`arm64`, check it against `checksums.txt`, and put the `aviary` binary on your
`PATH`. macOS quarantines a binary downloaded by a browser; clear that with
`xattr -d com.apple.quarantine aviary`.

`aviary --version` prints the installed version.

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

When the work is done, remove the stack from inside the worktree and then the
worktree, or the other way round and let `aviary gc` find the stack:

```sh
aviary rm                      # inside the worktree
git worktree remove ../myrepo-feature-x

git worktree remove ../myrepo-feature-x
aviary gc                      # anywhere; removes every stack whose worktree is gone
```

A script that makes worktrees can do the same: `aviary up` after
`git worktree add`, and `aviary rm` in the worktree before
`git worktree remove`. Let a failing aviary command warn rather than fail the
script, so a worktree is never left half made or half removed over its stack.

### Claude Code

Claude Code makes worktrees for `claude --worktree`, for subagents with
`isolation: worktree` and for background sessions. Without hooks it creates
and removes them with git and aviary needs nothing: the first `aviary up` or
`aviary test` in the worktree creates the stack, and `aviary gc` removes it
once Claude Code has removed the worktree.

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

A `WorktreeCreate` hook replaces Claude Code's own `git worktree add`, so it
creates the worktree itself and prints its path as the last line of stdout;
everything else goes to stderr. This one makes the worktree where Claude Code
would, under `.claude/worktrees/<name>` on branch `worktree-<name>`, but from
the current `HEAD`:

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

- The session starts only once `aviary up` has returned, which waits until the
  stack is healthy. For a stack that is slow to start, or when most worktrees
  will not need one, leave `aviary up` out of the create hook and keep the
  remove hook: every subagent worktree goes through the same hooks.
- When a subagent finishes, Claude Code keeps the worktree the hook made for it
  and does not run `WorktreeRemove`. Remove it with `git worktree remove`, then
  run `aviary gc`.
- Claude Code never deletes the branch of a worktree a hook made. Delete
  `worktree-<name>` yourself once its work is merged or abandoned.

### Directories deleted by hand

A worktree directory deleted with `rm -rf`, or by a tool that knows nothing
about aviary, leaves its stack behind: containers, volumes and ports.
`aviary ls` marks it `(worktree missing)` (`"worktree_missing": true` with
`--json`), and `aviary gc` removes it whenever you get to it;
`aviary gc --dry-run` lists what it would remove. git keeps listing such a
worktree until `git worktree prune`.

## Coming from a compose wrapper

A per-checkout compose wrapper usually merges its compose files into
generated ones inside the checkout, publishes fixed host ports, and offers
verbs over `docker compose`. aviary keeps the compose files as they are,
generates nothing, gives every worktree its own ports, and maps the usual
verbs like this:

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

Sources are mounted by the compose file the definition uses for development.
Paths are relative to the worktree root, and an anonymous volume over a
dependency directory keeps the image's copy instead of the host's (see
[Old dependencies after pulling an image](#old-dependencies-after-pulling-an-image)):

```yaml
services:
  api:
    volumes:
      - ./src/api:/app
      - /app/node_modules
```

Fixed host ports are what aviary replaces, so anything that reached a service
at one, such as `localhost:5432` in a database client, now takes the port from
`aviary env`. Configuration that embeds a fixed host port, such as a callback
URL, does not follow the allocated port.

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
allocate new ports (see [Port already allocated](#port-already-allocated)).

A relative `XDG_STATE_HOME` is ignored. aviary refuses a registry file whose
`version` it does not read, or with an entry whose `project` is missing or
differs from its key; the error names the file and the entry to fix or delete.

## Troubleshooting

### Registry file errors

aviary refuses a registry file it cannot trust, and the error names the file:

- `registry version <n> is not supported`: when `<n>` is higher than the
  version the error says this aviary reads, a newer aviary wrote the file.
  Upgrade aviary; an older one never overwrites a newer file. Otherwise the
  `version` key was removed (the error then says version `0`) or edited by
  hand: restore `"version": 1` at the top level of the file.
- A JSON parse error: the file was edited by hand. Fix the JSON.
- `entry "<key>" has project "<name>"`: set the entry's `project` to its key,
  or delete the entry.

Deleting an entry, or the whole file, makes aviary forget those stacks without
removing them. Their containers and volumes stay, and their names start with
`aviary-`. `docker compose ls --all` lists only stacks that still have
containers; a forgotten stack that was already down has only volumes, which
`docker volume ls --filter name=aviary-` lists (or
`docker volume ls --filter label=com.docker.compose.project=<project>` for one
stack). `docker compose --project-name <project> down --volumes` removes a
forgotten stack's containers and volumes either way. A forgotten stack whose
worktree still exists is registered again by the next command that starts it,
marked uninitialized, so init runs again over the data its volumes still hold;
run `aviary down --volumes` first to start clean.

### Port already allocated

aviary checks that a port is free only when it allocates it. If something
starts listening on one of a stack's ports later, `aviary up` fails with
compose's "port is already allocated" or "address already in use".
`aviary env` lists the stack's ports, and
`lsof -nP -iTCP:<port> -sTCP:LISTEN` shows what holds one.

Stop that process, or give the stack new ports with `aviary rm` and then
`aviary up`, which allocates again and skips ports in use. `aviary rm` also
removes the stack's volumes, so its data is lost and init runs again: aviary
cannot move a stack to new ports and keep its volumes, so free the port
instead when the data matters.

A compose file that publishes a fixed host port, such as `"5432:5432"`, fails
the same way in the second worktree that starts it. Publish
`${AVIARY_PORT_<NAME>:-5432}` instead (see [Ports](#ports)).

### Old dependencies after pulling an image

An anonymous volume over a dependency directory, such as `/app/node_modules`
under a source bind mount, outlives the container: when compose recreates the
container for a newer image, it keeps the old volume and so the old
dependencies. After `aviary compose pull` or `aviary build`, start the stack
with `aviary up --renew-anon-volumes` to refill those volumes from the new
image. `aviary run` and `aviary test` start a new container each time, with
new anonymous volumes, so they always see the image's copy.

### Running out of memory

Docker Desktop runs containers in a virtual machine with a fixed memory size,
set under Settings, Resources; on Linux the host's memory is the limit. Every
stack is a full copy of the services it runs, so several worktrees with the
whole application up cost several times one. When memory runs out, containers
are killed (exit status 137) or stop passing their health checks.
`docker stats --no-stream $(aviary ps -q)` shows what this stack uses.

Lay the definition out so that a bare `aviary up` starts only what most work
needs, usually the databases and caches, and put each heavier group of
services behind a [profile](#profiles). The same machine then holds one full
stack or several lean ones. Within a stack, start more only when needed:

- `aviary up <service>` starts a service even when it sits behind a profile.
- `aviary compose --profile <name> up --detach --wait` starts a whole profile.
  `COMPOSE_PROFILES=<name> aviary up` does the same only when `aviary.yml`
  sets no stack-wide profiles, because the `--profile` flags aviary passes
  override that variable.
- A suite starts only its `needs`, with its own profiles.

`aviary down` in worktrees not in use frees their memory and keeps their data.
A memory limit in a compose file (`deploy.resources.limits.memory`) caps one
service, so a runaway service is killed on its own instead of starving every
stack.

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
