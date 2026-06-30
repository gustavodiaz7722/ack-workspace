// Package refresher implements the Workspace_Refresher, which reconciles each
// Managed_Repository to a known-good baseline that is ready for development:
//
//   - the fork's default branch is synced with upstream (server-side, via the
//     GitHub merge-upstream API),
//   - all upstream tags are present on the local copy,
//   - the default branch ("main") is checked out, and
//   - the local default branch is reset to exactly match upstream (and thus the
//     fork).
//
// It is a declarative, idempotent operation: running it repeatedly converges on
// the same baseline. Reaching that baseline is intentionally destructive to
// local state on the default branch — uncommitted changes and untracked files
// are discarded and a diverged local default branch is reset — so confirmation
// of intent is the responsibility of the CLI layer. By the time Refresh is
// called the user has already opted in (or passed --dry-run). Work committed on
// other branches is left untouched.
package refresher
