// Package prereq verifies that the git executable, a GitHub token, and a GitHub
// identity -- whichever of the three a command declares -- are present before
// it performs any side-effecting work.
//
// Only those three are modeled here. A command that also needs make, helm,
// kubectl, eksctl, or AWS credentials reports a missing one as a failed Result
// when it runs, not as a pre-flight error.
//
// Each command declares which prerequisites it needs via a Need value. The
// Checker evaluates every requested prerequisite and aggregates all failures
// into a single error so the user sees every missing item at once.
package prereq

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
)

// Need declares the prerequisites a command requires. A command sets the fields
// that apply to it; the Checker evaluates only the requested prerequisites.
type Need struct {
	Git      bool // command performs git operations
	Token    bool // command performs GitHub API operations
	Identity bool // command requires GitHub identity
}

// Checker verifies that the prerequisites declared by a Need are satisfied.
type Checker interface {
	// Check evaluates all requested needs and returns an error listing every
	// missing prerequisite, or nil when all requested prerequisites are present.
	Check(need Need, cfg config.Config) error
}

// MissingError reports one or more missing prerequisites. It lists every
// missing item so the user can resolve them all at once.
type MissingError struct {
	// Missing holds a human-readable instruction for each missing prerequisite, in
	// evaluation order (git, token, identity).
	Missing []string
}

func (e *MissingError) Error() string {
	if len(e.Missing) == 1 {
		return "missing prerequisite: " + e.Missing[0]
	}
	return fmt.Sprintf("missing %d prerequisites:\n  - %s",
		len(e.Missing), strings.Join(e.Missing, "\n  - "))
}

// checker is the default Checker implementation. LookPath is injectable so
// tests can stub git resolution without touching the real PATH.
type checker struct {
	// LookPath resolves an executable on the PATH. It defaults to exec.LookPath.
	// Tests may stub it.
	LookPath func(file string) (string, error)
}

// NewChecker returns a Checker that resolves git via exec.LookPath.
func NewChecker() Checker {
	return &checker{LookPath: exec.LookPath}
}

// NewCheckerWithLookPath returns a Checker that resolves executables using the
// provided lookPath func. It is primarily intended for tests that need to stub
// git resolution without relying on the real PATH.
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

	// Git: a `git` executable must be resolvable on the PATH.
	if need.Git {
		if _, err := lookPath("git"); err != nil {
			missing = append(missing,
				"git: no `git` executable was found on your PATH; install git and ensure it is on your PATH")
		}
	}

	// Token: a non-empty GitHub token must be available.
	if need.Token {
		if strings.TrimSpace(cfg.Token) == "" {
			missing = append(missing,
				"GitHub token: no GitHub token was supplied; set the GITHUB_TOKEN environment variable or pass --token")
		}
	}

	// Identity: a non-empty GitHub identity must be configured.
	if need.Identity {
		if strings.TrimSpace(cfg.GitHubUser) == "" {
			missing = append(missing,
				"GitHub identity: no GitHub username is configured; pass --github-user, set GITHUB_USER, or save it in your configuration file")
		}
	}

	if len(missing) > 0 {
		return &MissingError{Missing: missing}
	}
	return nil
}
