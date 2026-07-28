package attributor

// This file is the only place in the package that imports the AWS SDK. Keeping
// the SDK behind the Backend interface is what lets the component's flow
// (provisioning reporting, polling, artifact validation, atomic file writing) be
// unit-tested with an in-memory fake and no credentials. It mirrors how the
// scanner package isolates Amazon Bedrock in bedrock.go.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	cbtypes "github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const (
	// inlinePolicyName is the name of the inline policy attached to the build
	// role. Writing a named inline policy (rather than attaching a managed one)
	// keeps provisioning idempotent and self-healing: re-running the command
	// rewrites exactly this document and nothing else.
	inlinePolicyName = "ack-workspace-attribution"
	// projectTimeoutMinutes bounds the build inside CodeBuild itself, so a wedged
	// build is reaped even if the local command is interrupted.
	projectTimeoutMinutes = 30
	// rolePropagationTimeout bounds how long project creation is retried while a
	// freshly created IAM role propagates. A new role is not immediately
	// assumable, and CodeBuild rejects a project whose service role it cannot yet
	// validate. The superseded bootstrap script papered over this with an
	// unconditional `sleep 10`; retrying against the actual error is both faster
	// when the role already exists and more reliable when propagation is slow.
	rolePropagationTimeout = 90 * time.Second
	// rolePropagationInterval is the delay between those retries.
	rolePropagationInterval = 5 * time.Second
)

// codeBuildBackend is the production Backend: it drives CodeBuild for compute,
// S3 for artifact staging, and IAM/STS for the service role.
type codeBuildBackend struct {
	codebuild *codebuild.Client
	s3        *s3.Client
	iam       *iam.Client

	region    string
	accountID string
	partition string
}

// NewCodeBuildBackend builds the production Backend using the default AWS
// credential chain. region overrides the region resolved from the environment or
// shared config. It resolves the caller's account and partition eagerly so a
// credential problem surfaces here, before any resource is created.
func NewCodeBuildBackend(ctx context.Context, region string) (Backend, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if strings.TrimSpace(region) != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS configuration: %w", err)
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("no AWS region configured; pass --region or set AWS_REGION")
	}

	ident, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("resolving AWS account identity: %w", err)
	}

	return &codeBuildBackend{
		codebuild: codebuild.NewFromConfig(cfg),
		s3:        s3.NewFromConfig(cfg),
		iam:       iam.NewFromConfig(cfg),
		region:    cfg.Region,
		accountID: aws.ToString(ident.Account),
		partition: partitionOf(aws.ToString(ident.Arn)),
	}, nil
}

// EnsureInfrastructure provisions the role, bucket, and project in that order:
// the project needs the role's ARN, and the role's policy needs the bucket name,
// so the bucket name is resolved first. Every step is idempotent and nothing is
// ever deleted.
func (b *codeBuildBackend) EnsureInfrastructure(ctx context.Context, in Infrastructure) (Provisioned, error) {
	in = in.withDefaults()
	if in.Bucket == "" {
		in.Bucket = defaultBucketName(b.accountID, b.region)
	}

	prov := Provisioned{Project: in.Project, Bucket: in.Bucket}

	roleARN, createdRole, err := b.ensureRole(ctx, in)
	if err != nil {
		return Provisioned{}, err
	}
	prov.RoleARN = roleARN
	prov.CreatedRole = createdRole

	createdBucket, err := b.ensureBucket(ctx, in.Bucket)
	if err != nil {
		return Provisioned{}, err
	}
	prov.CreatedBucket = createdBucket

	createdProject, updatedProject, err := b.ensureProject(ctx, in, roleARN, createdRole)
	if err != nil {
		return Provisioned{}, err
	}
	prov.CreatedProject = createdProject
	prov.UpdatedProject = updatedProject

	return prov, nil
}

// ensureRole creates the build role when absent and (always) rewrites its inline
// policy so a changed bucket or project name is reflected without manual repair.
func (b *codeBuildBackend) ensureRole(ctx context.Context, in Infrastructure) (arn string, created bool, err error) {
	out, err := b.iam.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(in.Role)})
	switch {
	case err == nil:
		arn = aws.ToString(out.Role.Arn)
	case isNoSuchEntity(err):
		createOut, cErr := b.iam.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName:                 aws.String(in.Role),
			AssumeRolePolicyDocument: aws.String(trustPolicy()),
			Description:              aws.String("Runs attribution-gen for ACK controllers on behalf of ack-workspace"),
		})
		if cErr != nil {
			return "", false, fmt.Errorf("creating IAM role %s: %w", in.Role, cErr)
		}
		arn = aws.ToString(createOut.Role.Arn)
		created = true
	default:
		return "", false, fmt.Errorf("looking up IAM role %s: %w", in.Role, err)
	}

	policy := permissionPolicy(b.partition, b.region, b.accountID, in.Project, in.Bucket)
	if _, err := b.iam.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(in.Role),
		PolicyName:     aws.String(inlinePolicyName),
		PolicyDocument: aws.String(policy),
	}); err != nil {
		return "", false, fmt.Errorf("writing inline policy on IAM role %s: %w", in.Role, err)
	}
	return arn, created, nil
}

// ensureBucket creates the artifact bucket when absent, with public access
// blocked. The bucket holds documents derived from public repositories, but it is
// created private because it lives in the user's account and there is no reason
// for it to be reachable.
func (b *codeBuildBackend) ensureBucket(ctx context.Context, bucket string) (bool, error) {
	if _, err := b.s3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err == nil {
		return false, nil
	}

	input := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	// us-east-1 is the API's implicit default and rejects an explicit constraint.
	if b.region != "us-east-1" {
		input.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(b.region),
		}
	}
	if _, err := b.s3.CreateBucket(ctx, input); err != nil {
		// A concurrent run (or a pre-existing bucket HeadBucket could not see) is
		// not an error: the bucket exists and is ours.
		var owned *s3types.BucketAlreadyOwnedByYou
		var exists *s3types.BucketAlreadyExists
		if errors.As(err, &owned) || errors.As(err, &exists) {
			return false, nil
		}
		return false, fmt.Errorf("creating artifact bucket %s: %w", bucket, err)
	}

	if _, err := b.s3.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: aws.String(bucket),
		PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
			BlockPublicAcls:       aws.Bool(true),
			BlockPublicPolicy:     aws.Bool(true),
			IgnorePublicAcls:      aws.Bool(true),
			RestrictPublicBuckets: aws.Bool(true),
		},
	}); err != nil {
		return true, fmt.Errorf("blocking public access on bucket %s: %w", bucket, err)
	}
	return true, nil
}

// ensureProject creates the CodeBuild project when absent, or updates an existing
// one so its buildspec, image, and role match what this version of the tool
// expects.
//
// The project is generic: its source type is NO_SOURCE and the buildspec clones
// whichever repository a build names through environment overrides. That is why
// it never needs to be mutated per run, unlike the shared project the superseded
// bootstrap script rewrote for every repository.
func (b *codeBuildBackend) ensureProject(ctx context.Context, in Infrastructure, roleARN string, roleIsNew bool) (created, updated bool, err error) {
	source := &cbtypes.ProjectSource{
		Type:      cbtypes.SourceTypeNoSource,
		Buildspec: aws.String(in.buildspec()),
	}
	environment := &cbtypes.ProjectEnvironment{
		Type:        cbtypes.EnvironmentTypeLinuxContainer,
		Image:       aws.String(in.Image),
		ComputeType: cbtypes.ComputeType(in.ComputeType),
	}
	artifacts := &cbtypes.ProjectArtifacts{Type: cbtypes.ArtifactsTypeNoArtifacts}

	existing, err := b.codebuild.BatchGetProjects(ctx, &codebuild.BatchGetProjectsInput{
		Names: []string{in.Project},
	})
	if err != nil {
		return false, false, fmt.Errorf("looking up CodeBuild project %s: %w", in.Project, err)
	}

	if len(existing.Projects) > 0 {
		if _, err := b.codebuild.UpdateProject(ctx, &codebuild.UpdateProjectInput{
			Name:             aws.String(in.Project),
			Source:           source,
			Environment:      environment,
			Artifacts:        artifacts,
			ServiceRole:      aws.String(roleARN),
			TimeoutInMinutes: aws.Int32(projectTimeoutMinutes),
		}); err != nil {
			return false, false, fmt.Errorf("updating CodeBuild project %s: %w", in.Project, err)
		}
		return false, true, nil
	}

	create := func() error {
		_, err := b.codebuild.CreateProject(ctx, &codebuild.CreateProjectInput{
			Name:             aws.String(in.Project),
			Source:           source,
			Environment:      environment,
			Artifacts:        artifacts,
			ServiceRole:      aws.String(roleARN),
			TimeoutInMinutes: aws.Int32(projectTimeoutMinutes),
			Description:      aws.String("Generates ACK controller ATTRIBUTION.md files outside the corporate network"),
		})
		return err
	}

	if err := create(); err != nil {
		// A role created moments ago may not be assumable yet, which CodeBuild
		// reports as an invalid-input error naming the service role. Retry only
		// that case, and only when we just created the role.
		if !roleIsNew || !isRolePropagationError(err) {
			return false, false, fmt.Errorf("creating CodeBuild project %s: %w", in.Project, err)
		}
		deadline := time.Now().Add(rolePropagationTimeout)
		for {
			if err := sleepCtx(ctx, rolePropagationInterval); err != nil {
				return false, false, err
			}
			cErr := create()
			if cErr == nil {
				break
			}
			if !isRolePropagationError(cErr) || !time.Now().Before(deadline) {
				return false, false, fmt.Errorf("creating CodeBuild project %s: %w", in.Project, cErr)
			}
		}
	}
	return true, false, nil
}

// StartBuild launches one generation build, passing the repository, ref, and
// artifact destination as environment overrides.
func (b *codeBuildBackend) StartBuild(ctx context.Context, req BuildRequest) (string, error) {
	plaintext := func(name, value string) cbtypes.EnvironmentVariable {
		return cbtypes.EnvironmentVariable{
			Name:  aws.String(name),
			Value: aws.String(value),
			Type:  cbtypes.EnvironmentVariableTypePlaintext,
		}
	}
	out, err := b.codebuild.StartBuild(ctx, &codebuild.StartBuildInput{
		ProjectName: aws.String(req.Project),
		EnvironmentVariablesOverride: []cbtypes.EnvironmentVariable{
			plaintext(envRepoURL, req.RepoURL),
			plaintext(envRepoRef, req.Ref),
			plaintext(envArtifactBucket, req.Bucket),
			plaintext(envArtifactKey, req.Key),
		},
	})
	if err != nil {
		return "", err
	}
	if out.Build == nil {
		return "", fmt.Errorf("CodeBuild returned no build for project %s", req.Project)
	}
	return aws.ToString(out.Build.Id), nil
}

// Status reports one build's current state, phase, and log deep link.
func (b *codeBuildBackend) Status(ctx context.Context, buildID string) (BuildStatus, error) {
	out, err := b.codebuild.BatchGetBuilds(ctx, &codebuild.BatchGetBuildsInput{Ids: []string{buildID}})
	if err != nil {
		return BuildStatus{}, err
	}
	if len(out.Builds) == 0 {
		return BuildStatus{}, fmt.Errorf("build %s not found", buildID)
	}

	build := out.Builds[0]
	status := BuildStatus{
		State: BuildState(build.BuildStatus),
		Phase: aws.ToString(build.CurrentPhase),
	}
	if build.Logs != nil {
		status.LogsURL = aws.ToString(build.Logs.DeepLink)
	}
	return status, nil
}

// FetchArtifact downloads the staged document from S3. This replaces the
// superseded approach of reassembling the file from CloudWatch log events, so the
// bytes written locally are exactly the bytes the generator produced.
func (b *codeBuildBackend) FetchArtifact(ctx context.Context, bucket, key string) ([]byte, error) {
	out, err := b.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("reading s3://%s/%s: %w", bucket, key, err)
	}
	defer out.Body.Close()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("reading body of s3://%s/%s: %w", bucket, key, err)
	}
	return data, nil
}

// isNoSuchEntity reports whether err is IAM's "no such entity" error.
func isNoSuchEntity(err error) bool {
	var nse *iamtypes.NoSuchEntityException
	return errors.As(err, &nse)
}

// isRolePropagationError reports whether err looks like CodeBuild rejecting a
// service role that has not finished propagating. The condition is only
// detectable from the message, so the match is deliberately narrow: an
// invalid-input error that mentions the service role.
func isRolePropagationError(err error) bool {
	var invalid *cbtypes.InvalidInputException
	if !errors.As(err, &invalid) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "service role") ||
		strings.Contains(msg, "cannot be assumed") ||
		strings.Contains(msg, "not authorized")
}

// partitionOf extracts the ARN partition from the caller's identity ARN,
// defaulting to the commercial partition when it cannot be determined, so policy
// ARNs are correct in China and GovCloud regions too.
func partitionOf(arn string) string {
	parts := strings.SplitN(arn, ":", 3)
	if len(parts) >= 2 && parts[0] == "arn" && parts[1] != "" {
		return parts[1]
	}
	return "aws"
}
