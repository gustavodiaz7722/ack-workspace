// Package initializer forks, clones, and configures the core ACK repositories
// (runtime, code-generator, test-infra, and ack-dev-skills).
//
// Init ensures the workspace root exists first (failing fast if it cannot be
// created), then processes each core repository concurrently, recording each in
// exactly one of the created, skipped, or failed buckets of the returned
// Summary.
package initializer
