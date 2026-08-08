package attributor

import "context"

// BuildState is the terminal-or-not state of one remote generation build. The
// values mirror CodeBuild's buildStatus vocabulary, but are redeclared here so
// the component, its tests, and the CLI layer stay free of any AWS SDK
// dependency (only codebuild.go imports the AWS SDK).
type BuildState string

const (
	// StateInProgress means the build is still running.
	StateInProgress BuildState = "IN_PROGRESS"
	// StateSucceeded means the build completed and the artifact should exist.
	StateSucceeded BuildState = "SUCCEEDED"
	// StateFailed means the build ran but a phase failed (for example
	// attribution-gen returned non-zero, or the repository ref could not be
	// fetched).
	StateFailed BuildState = "FAILED"
	// StateStopped means the build was stopped, typically by a human.
	StateStopped BuildState = "STOPPED"
	// StateFault means CodeBuild itself failed to run the build.
	StateFault BuildState = "FAULT"
	// StateTimedOut means the build exceeded CodeBuild's own timeout.
	StateTimedOut BuildState = "TIMED_OUT"
)

// Terminal reports whether the state is final, so the poll loop can stop. An
// unrecognized state is treated as terminal rather than in-progress: waiting
// forever on a state this code does not understand is worse than reporting it.
func (s BuildState) Terminal() bool {
	return s != StateInProgress
}

// OK reports whether the build finished successfully.
func (s BuildState) OK() bool {
	return s == StateSucceeded
}

// Infrastructure names the remote compute the generator runs on. Every field is
// optional at the CLI boundary; the backend fills in an account-scoped default
// for any name left empty and reports what it resolved in Provisioned.
type Infrastructure struct {
	// Project is the CodeBuild project name.
	Project string
	// Role is the IAM role name CodeBuild assumes. An existing role is reused;
	// only the tool's own named inline policy is rewritten, so a changed bucket or
	// project name is picked up without manual repair.
	Role string
	// Bucket is the S3 bucket the generated document is staged in.
	Bucket string
	// Image is the CodeBuild container image.
	Image string
	// GoVersion is the golang runtime version requested in the buildspec. It must
	// be one the Image actually provides.
	GoVersion string
	// ComputeType is the CodeBuild compute size.
	ComputeType string
}

// Provisioned reports the resolved infrastructure names and which resources had
// to be created. The created flags exist so the command can tell the user
// exactly what it added to their AWS account rather than provisioning silently.
type Provisioned struct {
	// Project, Bucket, and RoleARN are the resolved (possibly defaulted) names.
	Project string
	Bucket  string
	RoleARN string

	// CreatedRole, CreatedBucket, and CreatedProject report first-time creation.
	CreatedRole    bool
	CreatedBucket  bool
	CreatedProject bool
	// UpdatedProject reports that an existing project's configuration was brought
	// in line with the current buildspec.
	UpdatedProject bool
}

// Created reports whether provisioning had to create or change anything, so a
// no-op run can stay quiet about infrastructure.
func (p Provisioned) Created() bool {
	return p.CreatedRole || p.CreatedBucket || p.CreatedProject || p.UpdatedProject
}

// BuildRequest is one remote generation run: clone RepoURL at Ref, generate the
// attribution document, and stage it at Bucket/Key.
//
// The repository and ref travel as per-build environment overrides rather than
// as the project's own source configuration, so one immutable project serves
// every controller and every ref and concurrent runs cannot race by rewriting
// it.
type BuildRequest struct {
	Project string
	RepoURL string
	Ref     string
	Bucket  string
	Key     string
}

// BuildStatus is a poll observation of a running or finished build.
type BuildStatus struct {
	// State is the build's current state.
	State BuildState
	// Phase is the current or last build phase (for example "DOWNLOAD_SOURCE",
	// "BUILD"). It is reported on failure so the user learns which step broke
	// without reading logs.
	Phase string
	// LogsURL deep-links to the build's CloudWatch log stream. Logs are for human
	// diagnosis only; the document itself travels through S3 (see the package
	// doc).
	LogsURL string
}

// Backend is the seam between the component and AWS. The production
// implementation (codebuild.go) drives CodeBuild, S3, IAM, and STS; tests
// substitute an in-memory fake so the whole flow (provisioning, polling,
// artifact validation, and file writing) is exercised without AWS credentials
// or network access.
type Backend interface {
	// EnsureInfrastructure idempotently provisions the IAM role, artifact bucket,
	// and CodeBuild project described by in, and returns the resolved names. It
	// must be safe to call repeatedly and must never delete anything.
	EnsureInfrastructure(ctx context.Context, in Infrastructure) (Provisioned, error)
	// StartBuild launches one generation build and returns its identifier.
	StartBuild(ctx context.Context, req BuildRequest) (string, error)
	// Status returns the current status of the identified build.
	Status(ctx context.Context, buildID string) (BuildStatus, error)
	// FetchArtifact downloads the staged document.
	FetchArtifact(ctx context.Context, bucket, key string) ([]byte, error)
}
