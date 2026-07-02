package scanner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// grep tuning constants.
const (
	// defaultGrepContext is the number of surrounding lines (grep -C) included
	// when the caller does not specify one.
	defaultGrepContext = 2
	// maxGrepContext bounds the requested context so a single call cannot pull
	// back an unbounded slice of a source.
	maxGrepContext = 25
)

// grepBinary is the search program invoked by grepContent. It is a var so it can
// be overridden (for example to point at an absolute path) if needed.
var grepBinary = "grep"

// grepOptions configures a grepContent call.
type grepOptions struct {
	contextLines int  // surrounding lines per match (grep -C); 0 for matches only
	ignoreCase   bool // case-insensitive matching (grep -i)
	lineNumbers  bool // prefix each output line with its line number (grep -n)
}

// grepContent runs the system grep over content and returns its output. Rather
// than reimplement search, it shells out to the real grep. It is safe by
// construction: content is fed on stdin (no temp file, no path argument), and
// the pattern is passed as a single argument via -e with no shell involved.
//
// grep exit status 1 means "no matches" and is returned as empty output with a
// nil error; status >= 2 (for example an invalid regular expression) is an
// error the caller can relay to the model to correct.
func grepContent(ctx context.Context, content, pattern string, opts grepOptions) (string, error) {
	if strings.TrimSpace(pattern) == "" {
		return "", fmt.Errorf("pattern must not be empty")
	}
	contextLines := opts.contextLines
	if contextLines < 0 {
		contextLines = 0
	}
	if contextLines > maxGrepContext {
		contextLines = maxGrepContext
	}

	args := []string{"-E"}
	if opts.lineNumbers {
		args = append(args, "-n")
	}
	if contextLines > 0 {
		args = append(args, "-C", strconv.Itoa(contextLines))
	}
	if opts.ignoreCase {
		args = append(args, "-i")
	}
	args = append(args, "-e", pattern)

	cmd := exec.CommandContext(ctx, grepBinary, args...)
	cmd.Stdin = strings.NewReader(content)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if exitErr.ExitCode() == 1 {
				return "", nil // no lines matched
			}
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return "", fmt.Errorf("grep: %s", msg)
			}
		}
		return "", fmt.Errorf("running grep: %w", err)
	}
	return stdout.String(), nil
}
