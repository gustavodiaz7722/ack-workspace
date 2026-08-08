package attributor

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Default infrastructure names and sizing. Each is overridable from the command
// line; the defaults are shared across every controller because the project is
// generic (the repository and ref are per-build inputs, not project state).
const (
	// DefaultProject is the CodeBuild project name.
	DefaultProject = "ack-workspace-attribution"
	// DefaultRole is the IAM role CodeBuild assumes.
	DefaultRole = "ack-workspace-attribution-codebuild"
	// DefaultImage is the CodeBuild container image. It must provide the golang
	// runtime named by DefaultGoVersion.
	DefaultImage = "aws/codebuild/standard:7.0"
	// DefaultGoVersion is the golang runtime requested in the buildspec.
	//
	// It must be a version the image actually ships: standard:7.0 (Ubuntu 22.04)
	// offers golang 1.20 through 1.24, so 1.24 is the newest valid choice. The
	// superseded bootstrap script asked this image for golang 1.25, which does not
	// exist on it. GOTOOLCHAIN=auto in the buildspec covers the remaining risk by
	// letting Go fetch a newer toolchain if attribution-gen's own go.mod demands
	// one.
	DefaultGoVersion = "1.24"
	// DefaultComputeType is the CodeBuild compute size. Generation is a short,
	// single-threaded module walk, so the smallest instance is sufficient.
	DefaultComputeType = "BUILD_GENERAL1_SMALL"
	// DefaultBucketPrefix is the stem of the generated artifact bucket name. The
	// backend appends the account ID and region to make it globally unique.
	DefaultBucketPrefix = "ack-workspace-attribution"
)

// Build environment variable names. These are the contract between the inline
// buildspec below and the per-build overrides the backend sends, which is what
// lets one immutable project serve every controller and ref.
const (
	envRepoURL        = "REPO_URL"
	envRepoRef        = "REPO_REF"
	envArtifactBucket = "ARTIFACT_BUCKET"
	envArtifactKey    = "ARTIFACT_KEY"
)

// attributionHeader is the first line every attribution-gen document carries.
// It is used to validate a fetched artifact before it is allowed to overwrite a
// checked-in ATTRIBUTION.md.
//
// Refusing to write anything that does not start like a real attribution
// document turns a truncated or partial artifact into a loud failure rather
// than a plausible-looking file that silently replaces a good one.
const attributionHeader = "# Open Source Software Attribution"

// withDefaults returns a copy of in with every empty field filled from the
// package defaults. The bucket is left alone: only the backend can default it,
// because a globally unique name needs the caller's account ID and region.
func (in Infrastructure) withDefaults() Infrastructure {
	if in.Project == "" {
		in.Project = DefaultProject
	}
	if in.Role == "" {
		in.Role = DefaultRole
	}
	if in.Image == "" {
		in.Image = DefaultImage
	}
	if in.GoVersion == "" {
		in.GoVersion = DefaultGoVersion
	}
	if in.ComputeType == "" {
		in.ComputeType = DefaultComputeType
	}
	return in
}

// buildspec renders the inline buildspec the CodeBuild project runs.
//
// The project uses a NO_SOURCE source type and this buildspec clones the target
// repository itself, for two reasons. A GITHUB source type would tie the
// project to one repository and require CodeBuild-level GitHub credentials,
// whereas cloning in the build works for any public ACK repository with no
// setup. And it makes the repository and ref per-build inputs, so the project
// is never mutated between runs.
//
// The clone is a fetch of a single ref at depth 1 rather than `git clone
// --branch`, because the same form works for a branch, a tag, and a pull request
// head ref (refs/pull/N/head), which `--branch` does not.
//
// post_build runs even when build fails, so the `test -s` guard is what stops a
// failed generation from staging an empty or partial object.
func (in Infrastructure) buildspec() string {
	return fmt.Sprintf(`version: 0.2

env:
  variables:
    # Let Go fetch a newer toolchain if attribution-gen requires one.
    GOTOOLCHAIN: auto
    WORK_DIR: /tmp/ack-attribution
    SRC_DIR: /tmp/ack-attribution/src
    OUT_FILE: /tmp/ack-attribution/ATTRIBUTION.md

phases:
  install:
    runtime-versions:
      golang: "%s"
    commands:
      - export GOPATH="$HOME/go"
      - export PATH="$PATH:$GOPATH/bin"
      - go install github.com/awslabs/attribution-gen@latest
  pre_build:
    commands:
      - rm -rf "$WORK_DIR"
      - mkdir -p "$SRC_DIR"
      - cd "$SRC_DIR"
      - git init -q .
      - git remote add origin "$%s"
      - git fetch -q --depth 1 origin "$%s"
      - git checkout -q FETCH_HEAD
      - test -f "$SRC_DIR/go.mod"
  build:
    commands:
      - export GOPATH="$HOME/go"
      - export PATH="$PATH:$GOPATH/bin"
      - cd "$SRC_DIR"
      - attribution-gen --modfile "$SRC_DIR/go.mod" --output "$OUT_FILE"
  post_build:
    commands:
      - test -s "$OUT_FILE"
      - aws s3 cp "$OUT_FILE" "s3://$%s/$%s"
`, in.GoVersion, envRepoURL, envRepoRef, envArtifactBucket, envArtifactKey)
}

// trustPolicy is the assume-role policy that lets CodeBuild use the role.
func trustPolicy() string {
	doc := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":    "Allow",
			"Principal": map[string]string{"Service": "codebuild.amazonaws.com"},
			"Action":    "sts:AssumeRole",
		}},
	}
	return mustJSON(doc)
}

// permissionPolicy is the inline policy granting the build exactly what it
// needs: its own log group, and write access to the artifact bucket.
//
// Scoping to one log group and one bucket prefix keeps the role
// least-privilege; a managed policy such as CloudWatchLogsFullAccess would be
// far broader than the build needs.
func permissionPolicy(partition, region, accountID, project, bucket string) string {
	logGroup := fmt.Sprintf("arn:%s:logs:%s:%s:log-group:/aws/codebuild/%s",
		partition, region, accountID, project)
	doc := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{
				"Effect": "Allow",
				"Action": []string{
					"logs:CreateLogGroup",
					"logs:CreateLogStream",
					"logs:PutLogEvents",
				},
				"Resource": []string{logGroup, logGroup + ":*"},
			},
			{
				"Effect":   "Allow",
				"Action":   []string{"s3:PutObject"},
				"Resource": fmt.Sprintf("arn:%s:s3:::%s/*", partition, bucket),
			},
		},
	}
	return mustJSON(doc)
}

// mustJSON marshals a policy document. The inputs are package-local literals,
// so a marshal failure would be a programming error rather than a runtime
// condition.
func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("attributor: marshaling policy document: %v", err))
	}
	return string(data)
}

// defaultBucketName builds the globally unique artifact bucket name for an
// account and region.
func defaultBucketName(accountID, region string) string {
	return strings.ToLower(fmt.Sprintf("%s-%s-%s", DefaultBucketPrefix, accountID, region))
}

// validateDocument checks that data looks like a real attribution document
// before it is written over a checked-in file. An empty or header-less payload
// means the remote build staged something unusable, which is a failure rather
// than a document to keep.
func validateDocument(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("generated document is empty")
	}
	if !strings.HasPrefix(strings.TrimSpace(string(data)), attributionHeader) {
		return fmt.Errorf("generated document does not start with %q; refusing to overwrite the existing file", attributionHeader)
	}
	return nil
}
