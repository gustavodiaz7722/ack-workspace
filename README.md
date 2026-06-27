# ack-workspace

`ack-workspace` is a command-line tool that streamlines local workspace setup for
contributors to [AWS Controllers for Kubernetes (ACK)](https://github.com/aws-controllers-k8s).

ACK is spread across dozens of per-service controller repositories plus a few core
repositories, all hosted in the `github.com/aws-controllers-k8s` GitHub organization.
Contributors work fork-first: fork each repository to their personal account, clone the
fork into a Go source path, and add an `upstream` remote pointing back at the org.
Keeping dozens of forks current by hand is tedious and error-prone. `ack-workspace`
automates it.

## Features

- **`init`** — fork, clone, and configure the core ACK repositories
  (`runtime`, `code-generator`, `test-infra`, and `ack-dev-skills`).
- **`add`** — fork, clone, and configure one or more service controller repositories
  (or every controller in the ACK org with `add all`).
- **`sync`** — update managed forks from upstream across the whole workspace, using
  fast-forward-only merges so local work is never lost.
- **`status`** — report the state of every managed repository (branch, dirty flag,
  ahead/behind vs. upstream) as a table or JSON.
- **`config`** — view and persist your settings.

Built-in safety:

- **Never destroys local work** — sync is fast-forward-only and skips dirty or diverged
  repositories.
- **`--dry-run`** — preview exactly what every command would do without touching GitHub,
  git, or the filesystem.
- **Resilient & concurrent** — repositories are processed in parallel with a bounded
  worker pool; one failing repository never stops the batch.

## Installation

Requires Go 1.26+ and a `git` executable on your `PATH`.

Build from source:

```bash
git clone https://github.com/gustavodiaz7722/ack-workspace.git
cd ack-workspace
go build -o ack-workspace .
# optionally move it onto your PATH
mv ack-workspace ~/.local/bin/
```

Or install directly with the Go toolchain:

```bash
go install github.com/aws-controllers-k8s/ack-workspace@latest
```

## Prerequisites

Each command checks its prerequisites up front and fails fast with a clear message if
anything is missing:

| Command  | `git` | GitHub token | GitHub identity |
|----------|:-----:|:------------:|:---------------:|
| `init`   |  yes  |     yes      |       yes       |
| `add`    |  yes  |     yes      |       yes       |
| `sync`   |  yes  |      no¹     |       no        |
| `status` |  yes  |      no      |       no        |
| `config` |  no   |      no      |       no        |

¹ `sync` uses git remotes (which carry their own credentials), not the GitHub API.

Provide a GitHub token via the `--token` flag or the `GITHUB_TOKEN` environment variable.
The token is **never** written to the config file.

## Configuration

Settings are resolved with the following precedence, highest first:

1. command-line flag
2. environment variable (where one is defined)
3. persisted config file (`$HOME/.ack-workspace/config`)
4. built-in default

| Setting          | Flag                | Env            | Default                                                  |
|------------------|---------------------|----------------|----------------------------------------------------------|
| GitHub identity  | `--github-user`     | `GITHUB_USER`  | _(required)_                                             |
| GitHub token     | `--token`           | `GITHUB_TOKEN` | _(required for `init`/`add`; never persisted)_           |
| Workspace root   | `--workspace-root`  | —              | `$GOPATH/src/github.com/aws-controllers-k8s`             |
| Fork name prefix | `--prefix`          | —              | `ack-`                                                   |
| Concurrency      | `--concurrency`     | —              | `4` (valid range: `1`–`32`)                              |
| Preview mode     | `--dry-run`         | —              | `false`                                                  |

Save your settings once so you don't repeat them:

```bash
export GITHUB_TOKEN=ghp_xxx
ack-workspace config set --github-user octocat
ack-workspace config get      # print the resolved values
ack-workspace config path     # print the config file path
```

## Usage

### Initialize a workspace

Fork, clone, and configure the core ACK repositories — `runtime`, `code-generator`,
`test-infra`, and `ack-dev-skills`:

```bash
ack-workspace init
```

`ack-dev-skills` packages the ACK development guidance as an
[Agent Skill](https://agentskills.io). It lands as a peer next to the other core repos
in your workspace root; point your AI tool at it to install the skill (see that repo's
README for tool-specific steps, e.g. Kiro:
`ln -s <workspace-root>/ack-dev-skills/skills/ack-dev ~/.kiro/skills/ack-dev`).

### Add service controllers

Accepts a bare service alias or the full `<alias>-controller` form:

```bash
ack-workspace add s3 sns
ack-workspace add dynamodb-controller
```

Set up **every** controller in the ACK organization with the special `all` identifier.
It discovers all `*-controller` repositories in `aws-controllers-k8s` and forks, clones,
and configures each one (archived repositories are skipped):

```bash
ack-workspace add all
```

When `all` is given it supersedes any other identifiers. Pair it with `--dry-run` to see
the full list first:

```bash
ack-workspace add all --dry-run
```

### Sync forks with upstream

Update every managed repository (fast-forward only):

```bash
ack-workspace sync
```

Sync a subset, and push the updated branches to your fork:

```bash
ack-workspace sync runtime s3-controller --push
```

Repositories with uncommitted changes are skipped ("uncommitted changes") and
repositories whose history has diverged are skipped ("diverged history"); their local
branches are left untouched.

### Inspect workspace status

```bash
ack-workspace status
ack-workspace status --json
```

### Preview any command

Add `--dry-run` to see what would happen without making any change:

```bash
ack-workspace init --dry-run
ack-workspace sync --dry-run
```

## Exit codes

- `0` — the command completed and no repository failed (dry-run always exits `0`).
- `1` — a pre-flight error occurred, or at least one repository failed.
- `2` — a usage/validation error (for example an out-of-range `--concurrency`, or `add`
  with no identifiers).

## How forks are named

Forks are created as `<prefix><upstream-name>` (default prefix `ack-`) under your account.
The local checkout directory uses the unprefixed `<upstream-name>` so it matches the
conventional ACK Go import path, and the `upstream` remote always points at
`aws-controllers-k8s/<upstream-name>`.

## Development

```bash
go build ./...                      # compile
go vet ./...                        # static analysis
go test ./...                       # unit tests (fast, hermetic)
go test -tags integration ./...     # + end-to-end tests against a real local git
```

The codebase is interface-driven: the GitHub API and `git` are accessed through small
interfaces with mocks, so the unit suite runs without network or real GitHub access.

## License

See [LICENSE](LICENSE).
