// Package cmd contains the cobra command definitions for ack-workspace
// (root, init, add, refresh, status, config). The commands stay thin: they
// resolve configuration, run prerequisite checks, delegate to an internal
// component, render the returned Summary, and translate it to an exit code.
//
// Command implementations are added in the CLI wiring task.
package cmd
