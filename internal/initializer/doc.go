// Package initializer implements the Workspace_Initializer, which forks,
// clones, and configures the three Common_Repositories (runtime,
// code-generator, test-infra).
//
// Init ensures the Workspace_Root exists first (failing fast if it cannot be
// created), then processes each Common_Repository concurrently, recording each
// in exactly one of the created, skipped, or failed buckets of the returned
// Summary.
package initializer
