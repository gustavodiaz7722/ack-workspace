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
- **`remove`** — delete a controller's local clone and GitHub fork (or every managed
  controller with `remove all`). Destructive; requires confirmation.
- **`refresh`** — reconcile managed repositories to a clean, up-to-date baseline ready for
  development: sync the fork from upstream, fetch all upstream tags, check out `main`, and
  reset `main` to match upstream. Destructive; requires confirmation.
- **`release`** — cut a release for a single service controller: update its base branch
  from upstream, create a `release-<version>` branch, regenerate the release artifacts,
  commit and push them to your fork, and open a pull request against upstream.
- **`status`** — report the state of every managed repository (branch, dirty flag,
  ahead/behind vs. upstream) as a table or JSON.
- **`scan`** — investigate known issues in managed controllers with an Amazon Bedrock,
  tool-using agent, and report structured, per-field findings (a table or JSON).
- **`config`** — view and persist your settings.

Built-in safety:

- **Destructive commands confirm first** — `refresh` and `remove` discard local state, so
  they require an interactive confirmation (or `--yes`); work committed on other branches
  is left intact.
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
| `remove` |  yes  |     yes      |       yes       |
| `release`|  yes  |     yes¹     |       yes       |
| `refresh`|  yes  |     yes²     |       yes       |
| `status` |  yes  |      no      |       no        |
| `scan`   |  no³  |      no      |       no        |
| `config` |  no   |      no      |       no        |

¹ `release` needs a token to open the upstream pull request and your identity to name the
fork branch; pass `--skip-pr` to push the release branch without opening a PR.

² `refresh` needs a token and identity to sync your fork from upstream via the GitHub API.

³ `scan` instead needs **AWS credentials** for Amazon Bedrock (resolved from the default
AWS credential chain) and a `grep` executable on your `PATH`. A `GITHUB_TOKEN`, if present,
is used to raise the rate limit when listing Terraform provider docs, but is not required.

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

### Remove controllers (destructive)

The inverse of `add`: permanently delete a controller's local clone **and** its GitHub
fork. Accepts a bare alias or the full form, or `all` to remove every managed controller
found under the workspace root:

```bash
ack-workspace remove s3
ack-workspace remove s3 sns-controller
ack-workspace remove all
```

This cannot be undone — a deleted fork is gone for good. Safeguards:

- It only ever deletes a fork owned by **your** GitHub identity; it refuses to touch the
  upstream `aws-controllers-k8s` organization.
- You are prompted to type `yes` before anything is deleted. Pass `--yes` to skip the
  prompt (for scripts).
- Repositories with uncommitted changes are skipped unless you pass `--force`.
- `--keep-fork` deletes only the local clone and leaves the fork intact.
- `--dry-run` previews exactly what would be deleted without touching anything.

```bash
ack-workspace remove all --dry-run        # preview
ack-workspace remove s3 --keep-fork        # delete local clone only
ack-workspace remove s3 --yes --force      # non-interactive, even if dirty
```

### Refresh repositories for development (destructive)

Reconcile every managed repository to a known-good baseline ready for development. For
each repository `refresh`:

1. syncs your fork's `main` from upstream server-side (GitHub merge-upstream),
2. fetches all upstream tags into the local copy,
3. discards uncommitted changes and untracked files,
4. checks out `main`, and
5. resets `main` to exactly match upstream (and therefore your fork).

```bash
ack-workspace refresh                          # all repositories (prompts for confirmation)
ack-workspace refresh runtime s3-controller    # a subset
ack-workspace refresh --dry-run                # preview; touches nothing
ack-workspace refresh --yes                    # skip the confirmation prompt
```

The end state per repository is: `main` checked out, your fork's `main` up to date with
upstream, the local `main` up to date with both, and every upstream tag present locally.

This permanently discards uncommitted changes and untracked files and resets a diverged
local `main`, so it asks for confirmation unless `--dry-run` or `--yes` is given. Work
committed on other branches is left intact.

### Cut a controller release

Mechanize the ACK controller release workflow for a single service controller. The
controller and the `code-generator` must already be present in your workspace (run `init`
and `add` first):

```bash
ack-workspace release ecr --version v1.0.1
```

This will, on the controller:

1. update the base branch (`main` by default) from `upstream`,
2. create a branch named `release-v1.0.1`,
3. regenerate the release artifacts by running the code-generator's
   `./scripts/build-controller-release.sh ecr` with `RELEASE_VERSION=v1.0.1`,
4. commit the artifacts as `Release artifacts for release v1.0.1`,
5. push the branch to your fork (`origin`), and
6. open a pull request against `aws-controllers-k8s/ecr-controller`.

The service may be a bare alias (`ecr`) or its full form (`ecr-controller`), and the
version is normalized to carry a leading `v` (`1.0.1` and `v1.0.1` are equivalent). Useful
flags:

```bash
ack-workspace release ecr --version v1.0.1 --dry-run      # preview every step
ack-workspace release ecr --version v1.0.1 --skip-pr      # push the branch, no PR
ack-workspace release ecr --version v1.0.1 --base-branch release-1.x
ack-workspace release ecr --version v1.0.1 --pr-body "$(cat notes.md)"   # custom PR body
```

Built-in safety: a controller with uncommitted changes is skipped, a base branch that has
diverged from upstream is reported as a failure (never force-updated), an existing
`release-<version>` branch is left untouched, and a release that generates no changes is
reported as a no-op instead of creating an empty commit.

### Inspect workspace status

```bash
ack-workspace status
ack-workspace status --json
```

### Scan controllers for known issues

`scan` runs an Amazon Bedrock, tool-using agent that investigates a known issue against a
single resource of a single controller and reports structured findings. Each
`(controller, resource, issue)` triple is one independent agent conversation; any of the
three dimensions may be `all` to fan out (conversations run in parallel, bounded by
`--concurrency`).

```bash
ack-workspace scan sns --resource Subscription --issue 1   # one triple
ack-workspace scan sns --resource all --issue 1            # every SNS resource
ack-workspace scan all                                     # every issue, resource, controller
```

The agent works from a small, sandboxed set of sources — a pre-filtered index of the
resource's CRD spec fields fused with its `generator.yaml` markings, and the resource's
Terraform provider docs — which it searches with `grep`. Each issue defines its own
pass/fail rule and a reduced summary, so results read as `PASS`/`FAIL` with only the
relevant field paths:

```
sns/Topic  issue 1 (json-document-fields)  FAIL
    incorrectly marked: dataProtectionPolicy (is none, expected is_document)
    correctly marked: deliveryPolicy, policy
    terraform-only (no CRD field): archive_policy
```

Currently one issue is available:

- **Issue 1 (`json-document-fields`)** — find CRD fields that hold a JSON/YAML or IAM
  policy document but are not marked `is_document` / `is_iam_policy` in `generator.yaml`.

Useful flags:

```bash
ack-workspace scan sns --resource Topic --issue 1 --json    # machine-readable findings
ack-workspace scan sns --resource Topic --issue 1 --debug   # full agent transcript on stderr (runs serially)
ack-workspace scan sns --issue 1 --model <bedrock-model-id> --region us-west-2
```

`--json` emits the full findings (including each finding's `terraform_field` and
`ack_field_path`); `--debug` prints the complete conversation — every prompt, tool call,
tool result, and the final report — to stderr, leaving stdout clean.

### Preview any command

Add `--dry-run` to see what would happen without making any change:

```bash
ack-workspace init --dry-run
ack-workspace refresh --dry-run
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
