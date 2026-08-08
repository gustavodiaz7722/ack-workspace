// Package prereq verifies that what a command needs is present before it
// performs any side-effecting work: the external executables it invokes, and the
// configuration values it cannot proceed without.
//
// Each command declares its requirements as a Need. The Checker evaluates all of
// them and aggregates the failures into a single error, so a user missing three
// things is told all three at once rather than discovering them one run at a
// time.
//
// What is deliberately NOT checked here is anything that needs a network call or
// credentials to answer — whether AWS credentials are valid, whether a token has
// the right scopes, whether a cluster is reachable. Keeping the checker hermetic
// is what lets it run on every invocation without adding latency, so those remain
// runtime concerns reported as a failed Result.
package prereq

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
)

// Tool identifies an external executable a command invokes. Tools are bit flags
// so a command can declare several by ORing them together, the set stays
// comparable, and a typo is a compile error rather than a check that silently
// never fires.
type Tool uint16

const (
	// Git is the git executable, used by every command that touches a repository.
	Git Tool = 1 << iota
	// Make drives the code-generator's build targets.
	Make
	// Go is the Go toolchain those targets invoke. It is declared alongside Make
	// because a missing go produces a confusing failure from inside make rather
	// than a clear one up front.
	Go
	// Docker builds and pushes controller images.
	Docker
	// AWS is the aws CLI, used for ECR, EKS, and IAM lookups.
	AWS
	// Kubectl manages the namespace and service account a controller runs under.
	Kubectl
	// Helm installs and upgrades a controller's chart.
	Helm
	// Eksctl creates the development cluster and its pod identity association.
	Eksctl
)

// tools lists every Tool with the executable name resolved on the PATH, in bit
// order. Iterating this rather than the flags directly keeps a missing-tool
// report in a stable, predictable order.
var tools = []struct {
	flag Tool
	name string
}{
	{Git, "git"},
	{Make, "make"},
	{Go, "go"},
	{Docker, "docker"},
	{AWS, "aws"},
	{Kubectl, "kubectl"},
	{Helm, "helm"},
	{Eksctl, "eksctl"},
}

// Need declares what a command requires. Tools must resolve on the PATH; Token
// and Identity must be present in the resolved configuration. A command sets
// only what applies to it, and the zero value requires nothing.
//
// A command declares every tool it can invoke, including one it may not reach on
// a given run. deploy names eksctl even though it only creates a cluster when
// none exists: a caller who cannot create one should learn that before the image
// is built, not twenty minutes in.
type Need struct {
	// Tools are the external executables that must resolve on the PATH.
	Tools Tool
	// Token requires a non-empty GitHub token (the command calls the GitHub API).
	Token bool
	// Identity requires a non-empty GitHub username (the command names a fork).
	Identity bool
}

// Checker verifies that the prerequisites declared by a Need are satisfied.
type Checker interface {
	// Check evaluates all requested needs and returns an error listing every
	// missing prerequisite, or nil when all requested prerequisites are present.
	Check(need Need, cfg config.Config) error
}

// MissingError reports one or more missing prerequisites. It lists every missing
// item so the user can resolve them all at once.
type MissingError struct {
	// Missing holds a human-readable instruction for each missing prerequisite, in
	// evaluation order (tools in bit order, then token, then identity).
	Missing []string
}

func (e *MissingError) Error() string {
	if len(e.Missing) == 1 {
		return "missing prerequisite: " + e.Missing[0]
	}
	return fmt.Sprintf("missing %d prerequisites:\n  - %s",
		len(e.Missing), strings.Join(e.Missing, "\n  - "))
}

// checker is the default Checker implementation. LookPath is injectable so tests
// can drive tool resolution without touching the real PATH.
type checker struct {
	// LookPath resolves an executable on the PATH. It defaults to exec.LookPath.
	LookPath func(file string) (string, error)
}

// NewChecker returns a Checker that resolves executables via exec.LookPath.
func NewChecker() Checker {
	return &checker{LookPath: exec.LookPath}
}

// NewCheckerWithLookPath returns a Checker that resolves executables using the
// provided lookPath func, for tests that need to script which tools are present.
func NewCheckerWithLookPath(lookPath func(file string) (string, error)) Checker {
	return &checker{LookPath: lookPath}
}

// Check evaluates every requested prerequisite and aggregates all failures into
// a single *MissingError. It returns nil only when all requested prerequisites
// are satisfied.
func (c *checker) Check(need Need, cfg config.Config) error {
	lookPath := c.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}

	var missing []string

	for _, t := range tools {
		if need.Tools&t.flag == 0 {
			continue
		}
		if _, err := lookPath(t.name); err != nil {
			missing = append(missing, fmt.Sprintf(
				"%s: no `%s` executable was found on your PATH; install it and ensure it is on your PATH",
				t.name, t.name))
		}
	}

	if need.Token && strings.TrimSpace(cfg.Token) == "" {
		missing = append(missing,
			"GitHub token: no GitHub token was supplied; set the GITHUB_TOKEN environment variable or pass --token")
	}

	if need.Identity && strings.TrimSpace(cfg.GitHubUser) == "" {
		missing = append(missing,
			"GitHub identity: no GitHub username is configured; pass --github-user, set GITHUB_USER, or save it in your configuration file")
	}

	if len(missing) > 0 {
		return &MissingError{Missing: missing}
	}
	return nil
}
