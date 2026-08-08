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
- **`deploy`** — build a single service controller from your local checkout and deploy it to
  the long-lived ACK development cluster (`ack-dev-auto`), **creating that cluster on the first
  run** and reusing it thereafter. The cluster is fixed and your kubeconfig is repointed at it
  every run, so a deploy cannot land somewhere unintended. Requires `docker`, `aws`, `kubectl`,
  `helm`, and `eksctl` on your `PATH`.
- **`build`** — regenerate a single service controller's code from its local checked-out
  branch by running the code-generator's `make build-controller` target. Wires up the
  environment overrides (`RUNTIME_CRD_DIR`, `ACK_GENERATE_BIN_PATH`, `TEMPLATES_DIR`) that
  the code-generator scripts otherwise resolve relative to a workspace root literally named
  `aws-controllers-k8s`, so the build succeeds from any workspace root.
- **`status`** — report the state of every managed repository (branch, dirty flag,
  ahead/behind vs. upstream) as a table or JSON.
- **`candidates`** — emit the deterministic cross-resource-reference candidate index for a
  resource: every string-valued CRD spec field, fused with the generator.yaml markings that
  bear on whether it is a reference and with the API model's documentation and validation
  patterns.
- **`attribution`** — regenerate a controller's `ATTRIBUTION.md` by running the upstream
  `attribution-gen` tool on ephemeral AWS CodeBuild compute, then write the result into
  your local checkout. The work runs remotely because generating the document needs the
  public Go module proxy, which is unreachable from the Amazon corporate network.
- **`config`** — view and persist your settings.

Built-in safety:

- **Destructive commands confirm first** — `refresh` and `remove` discard local state, so
  they require an interactive confirmation (or `--yes`); work committed on other branches
  is left intact.
- **`--dry-run`** — preview what a command would do without changing GitHub, git, the
  filesystem, or any cloud resource. Read-only lookups (resolving your AWS account, checking
  whether the cluster exists) still run, so the preview reflects reality.
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

Every command declares what it needs and it is all verified **before any work starts**. If
several things are missing you get one error naming all of them, rather than discovering them
one run at a time:

```
missing 4 prerequisites:
  - aws: no `aws` executable was found on your PATH; install it and ensure it is on your PATH
  - kubectl: no `kubectl` executable was found on your PATH; install it and ensure it is on your PATH
  - helm: no `helm` executable was found on your PATH; install it and ensure it is on your PATH
  - eksctl: no `eksctl` executable was found on your PATH; install it and ensure it is on your PATH
```

| Command | Executables on `PATH` | GitHub token | GitHub identity |
|---------|-----------------------|:------------:|:---------------:|
| `init`   | `git` | yes | yes |
| `add`    | `git` | yes | yes |
| `remove` | `git` | yes | yes |
| `refresh`| `git` | yes¹ | yes |
| `release`| `git`² | yes³ | yes |
| `build`  | `git`, `make`, `go`⁴ | no | no |
| `deploy` | `git`, `docker`, `aws`, `kubectl`, `helm`, `eksctl`⁵ | no | no |
| `status` | `git` | no | no |
| `attribution` | `git`⁶ | no | no |
| `candidates` | — ⁷ | no | no |
| `config` | — | no | no |

¹ `refresh` needs a token and identity to sync your fork from upstream via the GitHub API.

² `release` also runs the code-generator's release script, which needs the code-generator's
own build dependencies (`go`, `make`, `controller-gen`, `helm`). Those are not pre-flighted,
because the script — not this tool — decides which of them it uses.

³ `release` needs a token to open the upstream pull request and your identity to name the
fork branch; pass `--skip-pr` to push the release branch without opening a PR.

⁴ `go` is checked alongside `make` because the `make` target invokes it, and a missing `go`
would otherwise surface as a confusing failure from inside `make`.

⁵ `eksctl` is required even though `deploy` only uses it when the development cluster has to
be created: learning you cannot create one is worth knowing before a 20-minute image build,
not after.

⁶ `attribution` runs the generation on CodeBuild through the AWS SDK, so it needs no AWS CLI.

⁷ `candidates` shells out to nothing. It reads local repositories and fetches the public AWS
API models over HTTPS.

**AWS credentials are not pre-flighted.** `deploy` and `attribution` resolve them from the
default chain when they run, so an expired session surfaces as a failure at that point rather
than up front. The same goes for anything else that needs a network round-trip to answer:
whether a token's scopes suffice, whether a cluster is reachable. Keeping the prerequisite
check hermetic is what lets it run on every invocation for free.

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
| GitHub identity  | `--github-user`     | `GITHUB_USER`  | _(none; required only by the commands that name a fork)_  |
| GitHub token     | `--token`           | `GITHUB_TOKEN` | _(none; required by the commands that call the GitHub API; never persisted)_ |
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

### Build a controller from local source

Regenerate a single service controller's code from its **local checked-out branch** by
running the code-generator's `make build-controller` target. Use this to regenerate a
controller after editing its `generator.yaml` or hook templates. The controller and the
`code-generator` must already be present in your workspace (run `init` and `add` first),
and the `make`/`go` toolchain must be on your `PATH`:

```bash
ack-workspace build ecr
```

This runs `make build-controller SERVICE=ecr` in the `code-generator` directory against
whatever the controller repository currently has checked out — it never switches branches
or touches git history.

Crucially, `build` wires up the environment overrides the code-generator scripts need when
your workspace root is **not** literally named `aws-controllers-k8s`. Those scripts default
`RUNTIME_CRD_DIR`, `ACK_GENERATE_BIN_PATH`, and `TEMPLATES_DIR` to paths relative to a
grandparent directory named `aws-controllers-k8s`, so a workspace rooted anywhere else
otherwise fails with `No such file or directory` or `Unable to find an ack-generate
binary`. `build` resolves all three against your real `--workspace-root` so the full build
(code, CRDs, RBAC, and Helm chart) succeeds regardless of the root's name.

The service may be a bare alias (`ecr`) or its full form (`ecr-controller`). By default the
aws-sdk-go version is read from the controller's `apis/<version>/ack-generate-metadata.yaml`;
pass `--sdk-version` to pin it. Useful flags:

```bash
ack-workspace build ecr --dry-run              # print the command that would run; builds nothing
ack-workspace build ecr --sdk-version v1.41.0  # pin the aws-sdk-go version
```

### Build and deploy a controller from local source

Build a single service controller from your **local checkout** and deploy it to the ACK
development cluster. Use this to test in-progress changes on a real cluster. The
controller and the `code-generator` must already be present in your workspace (run `init` and
`add` first), and `docker`, `aws`, `kubectl`, `helm`, and `eksctl` must be on your `PATH`:

```bash
ack-workspace deploy ecr
```

This will:

1. resolve your AWS account and region from the active AWS credentials,
2. bring the `ack-dev-auto` cluster into the state a controller needs — **creating it when
   absent** — and repoint your kubeconfig at it (see
   [the development cluster](#the-development-cluster) below),
3. ensure an ECR repository (`ecr-controller` by default) exists in that account,
   **creating it when absent**,
4. build the controller image from your checked-out source by running the code-generator's
   `./scripts/build-controller-image.sh ecr`, tagging it
   `<account>.dkr.ecr.<region>.amazonaws.com/ecr-controller:<HEAD-sha>`,
5. push the image to ECR (`aws ecr get-login-password` → `docker login` → `docker push`),
   and
6. `helm upgrade --install ack-ecr-controller <controller>/helm` into the `ack-system`
   namespace, pointing the deployment at the freshly pushed image.

The service may be a bare alias (`ecr`) or its full form (`ecr-controller`). By default the
image is tagged with the controller's checked-out HEAD short SHA, so each build is
traceable to the exact local commit. Useful flags:

```bash
ack-workspace deploy ecr --dry-run                     # preview every step; changes nothing
ack-workspace deploy ecr --image-tag dev               # use a fixed tag instead of the HEAD SHA
ack-workspace deploy ecr --namespace ack-test          # install into a different namespace
ack-workspace deploy ecr --repository my-ecr-controller  # override the ECR repository name
ack-workspace deploy ecr --region us-west-2            # target a specific region
ack-workspace deploy ecr --service-account ack-ecr-controller  # bind credentials to another account
```

#### The development cluster

Every deploy targets one cluster, `ack-dev-auto`, in the region resolved from your AWS
configuration. It is not selectable, and your current kubeconfig context is never used as-is:
`deploy` repoints the kubeconfig at `ack-dev-auto` on every run, so a deploy cannot land on a
cluster you did not intend. If the cluster does not exist, `deploy` creates it first, so the
first run doubles as a one-time bootstrap with nothing else to prepare — and every run after
that reuses it.

When the cluster is absent, `deploy` runs `eksctl create cluster` with a generated
configuration and then installs the controller:

- **EKS Auto Mode**, with the `general-purpose` and `system` node pools. Auto Mode makes
  compute, VPC CNI networking, EBS storage, load balancing and CoreDNS built-in cluster
  capabilities, so there are no node groups or addons to maintain. The EKS Pod Identity
  Agent is built in too, which is why the `eks-pod-identity-agent` addon must **not** be
  installed on such a cluster.
- **An EKS Pod Identity association** for `ack-system/ack-controller`, bound to an IAM role
  named `<cluster>-<namespace>-controller`. The pod gets
  `AWS_CONTAINER_CREDENTIALS_FULL_URI` and `AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE`
  injected, which the AWS SDK picks up on its own — no static secret and no
  `eks.amazonaws.com/role-arn` (IRSA) annotation.
- **The shared `ack-controller` service account**, which the controller is deployed under.
  Associations are keyed on `(namespace, serviceAccountName)` and do not support wildcards, so
  one shared account lets a single association cover every controller you deploy to the
  cluster. Name a different account with `--service-account` and an association is created for
  that one instead.

**The cluster is meant to be long-lived.** Create it once and keep it: every provisioning step
is idempotent, so each later deploy only fills in what is missing — including adding an
association for a service account that does not have one yet — and skips the 15–25 minute
creation entirely. A deploy onto the existing cluster is just build, push, and install, a few
minutes end to end. Multiple controllers coexist on it happily; they share one namespace, one
service account, and one association.

```bash
ack-workspace deploy ecr --dry-run              # preview, including any cluster creation
ack-workspace deploy ecr --cluster-version 1.34 # pin the k8s version if the cluster is created
ack-workspace deploy ecr --cluster-policy-arn arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly
```

`--cluster-version` and `--cluster-policy-arn` only apply when the cluster (or its IAM role)
has to be created, so they are ignored once it exists. Changing either afterwards means
editing the resource directly, or deleting it and letting the next deploy recreate it.

> **Caution:** the first deploy takes 15–25 minutes and creates billable AWS resources (an EKS
> cluster, its VPC, and an IAM role). A long-lived cluster keeps costing while it runs, so keep
> it in a development account you are happy to leave running. The pod identity role gets
> `AdministratorAccess` by default so that any ACK controller works without further setup —
> appropriate for a throwaway development account and nowhere else. Scope it down with
> `--cluster-policy-arn` in any account you share with others.

To delete it, remove your custom resources first so the controllers clean up the AWS resources
they created; those are not removed with the cluster.

```bash
eksctl delete cluster --name ack-dev-auto --region us-west-2
```

### Inspect workspace status

```bash
ack-workspace status
ack-workspace status --json
```

### Build the cross-resource-reference candidate index

`candidates` emits, as JSON Lines, every string-valued spec field of a resource's CRD fused
with the `generator.yaml` markings that bear on whether the field is a reference
(`is_reference` and its configured target, `is_immutable`, `is_primary_key`) and with the
service API model's field documentation and validation patterns.

It is the mechanical narrowing step of a reference audit: it produces the field set a
reviewer then judges. Model documentation is resolved by walking the model's shape graph to
each field path, which is what makes nested fields — undocumented in the CRD, and where
reference gaps concentrate — judgeable at all. Two runs over an unchanged repository produce
the same set, so an audit can be split across reviewers who all start from identical input.

```bash
ack-workspace candidates eks --resource Nodegroup                     # records on stdout
ack-workspace candidates all --resource all --out-dir /tmp/ref-audit  # one file per resource
```

Records go to stdout and progress plus `ignore.field_paths` suppression notes to stderr, so
stdout stays machine-readable. It reads local repositories and the public API models, so it
needs no AWS credentials, git, or GitHub identity. A model that cannot be fetched degrades
the affected records (`model_unavailable: true`) rather than failing the run.

### Generate a controller's ATTRIBUTION.md

`attribution` regenerates a controller's `ATTRIBUTION.md` with the upstream
[`attribution-gen`](https://github.com/awslabs/attribution-gen) tool and writes the result
into your local checkout.

The generation runs on ephemeral AWS CodeBuild compute, and that is a requirement rather
than an optimization: building the document walks the module dependency graph and fetches
every dependency from the public Go module proxy, which is blocked from inside the Amazon
corporate network. CodeBuild runs the generator outside that network.

```bash
ack-workspace attribution ecr                 # your fork, current branch
ack-workspace attribution ecr --ref pr/42     # a pull request
ack-workspace attribution ecr --upstream      # the aws-controllers-k8s org
ack-workspace attribution all                 # every managed controller
```

By default the build clones **your fork** at the controller's currently checked-out branch,
so push your work first — the build reads the remote and cannot see unpushed commits. The
command checks this before starting any compute and tells you if the ref is missing.

On first use it provisions three resources in your AWS account and reuses them afterwards:

| Resource | Default name | Purpose |
|---|---|---|
| IAM role | `ack-workspace-attribution-codebuild` | role CodeBuild assumes; scoped inline policy |
| S3 bucket | `ack-workspace-attribution-<account>-<region>` | stages the generated document |
| CodeBuild project | `ack-workspace-attribution` | runs `attribution-gen` |

The project is generic and immutable: its source type is `NO_SOURCE` and its buildspec
clones whichever repository a build names through environment overrides, so one project
serves every controller and concurrent runs never interfere.

Useful flags:

```bash
ack-workspace attribution ecr --dry-run                     # preview; provisions nothing
ack-workspace attribution ecr --output /tmp/ATTRIBUTION.md  # write elsewhere
ack-workspace attribution ecr --region us-west-2            # target a region
ack-workspace attribution ecr --repo https://github.com/me/fork
ack-workspace attribution ecr --image aws/codebuild/standard:8.0 --go-version 1.24
ack-workspace attribution ecr --timeout 30m
```

> **Caution:** this command creates an IAM role, an S3 bucket, and a CodeBuild project in
> whichever AWS account your credentials resolve to, and each run is a billable build.
> `--dry-run` previews all of it without creating anything. `attribution all` starts one
> build per controller.

A generated document is only written if it starts with `# Open Source Software Attribution`,
so a failed or truncated build can never overwrite a good checked-in file. The write is
atomic, and an unchanged document is reported as `already up to date` rather than rewritten.

If `--go-version` is overridden, it must be a runtime the chosen image actually ships. See
[AWS CodeBuild available runtimes](https://docs.aws.amazon.com/codebuild/latest/userguide/available-runtimes.html);
`standard:7.0` provides golang 1.20 through 1.24.

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
