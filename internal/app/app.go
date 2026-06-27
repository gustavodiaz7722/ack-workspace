// Package app defines the App context: the resolved configuration and the
// clients shared by every ack-workspace command.
//
// The App struct lives in this neutral internal package rather than in cmd so
// that the feature components (initializer, adder, syncer, inspector) can accept
// an App without importing the cmd package, which would create an import cycle
// (cmd imports the components, and the components would import cmd). The cmd
// layer constructs the App in cmd/root.go and passes it down to each component.
package app

import (
	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/githubclient"
)

// App carries the resolved configuration and the clients that the feature
// components need to perform their work. It is constructed once per invocation
// in cmd/root.go after configuration resolution and prerequisite checks succeed.
type App struct {
	// Config is the effective configuration resolved for this invocation
	// (flag > env > persisted > default).
	Config config.Config
	// GitHub performs GitHub API operations (resolve repositories, create forks).
	GitHub githubclient.GitHubClient
	// Git executes local git commands under the Workspace_Root.
	Git git.Runner
	// DryRun, when true, instructs components to compute and report the actions
	// they would take using only read-only operations, without mutating GitHub,
	// git, or the filesystem.
	DryRun bool
}
