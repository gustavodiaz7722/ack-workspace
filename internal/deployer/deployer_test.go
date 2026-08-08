// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the License is
// located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed on
// an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package deployer

import (
	"errors"
	"strings"
	"testing"
)

// errFailedLookup stands in for the non-zero exit of `aws ecr describe-images`,
// whose meaning is carried by the command's output rather than the error itself.
var errFailedLookup = errors.New("exit status 254")

// argAfter returns the argument immediately following the first occurrence of
// flag in args, and whether flag was found with a following value.
func argAfter(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// setFlagFor returns the flag ("--set" or "--set-string") used to pass the
// given "key=value" assignment, or "" if the assignment is not present.
func setFlagFor(args []string, assignment string) string {
	for i, a := range args {
		if a == assignment && i > 0 {
			return args[i-1]
		}
	}
	return ""
}

// hasArg reports whether args contains a.
func hasArg(args []string, a string) bool {
	for _, got := range args {
		if got == a {
			return true
		}
	}
	return false
}

func TestHelmUpgradeArgs_TagUsesSetString(t *testing.T) {
	// An all-digit commit SHA is the regression case: with plain --set, Helm
	// coerces it to a number and the chart's values schema rejects it with "got
	// number, want string".
	const tag = "4881291"
	args := helmUpgradeArgs(DeployParams{
		ChartDir:  "/charts/ecr",
		Namespace: "ack-system",
		Release:   "ack-ecr-controller",
		ImageRepo: "123456789012.dkr.ecr.us-west-2.amazonaws.com/ecr-controller",
		ImageTag:  tag,
		Region:    "us-west-2",
	})

	if got := setFlagFor(args, "image.tag="+tag); got != "--set-string" {
		t.Errorf("image.tag should be passed with --set-string, got %q", got)
	}

	// The tag value must be preserved verbatim.
	if _, ok := argAfter(args, "--set-string"); !ok {
		t.Fatalf("expected a --set-string flag with a following value in %v", args)
	}
}

func TestHelmUpgradeArgs_CoreArgs(t *testing.T) {
	args := helmUpgradeArgs(DeployParams{
		ChartDir:  "/charts/ecr",
		Namespace: "ack-test",
		Release:   "ack-ecr-controller",
		ImageRepo: "repo/ecr-controller",
		ImageTag:  "dev",
		Region:    "eu-central-1",
	})

	if len(args) < 4 || args[0] != "upgrade" || args[1] != "--install" {
		t.Fatalf("expected helm upgrade --install prefix, got %v", args)
	}
	if args[2] != "ack-ecr-controller" {
		t.Errorf("expected release name as third arg, got %q", args[2])
	}
	if args[3] != "/charts/ecr" {
		t.Errorf("expected chart dir as fourth arg, got %q", args[3])
	}
	if got, _ := argAfter(args, "--namespace"); got != "ack-test" {
		t.Errorf("expected namespace ack-test, got %q", got)
	}
	if setFlagFor(args, "image.repository=repo/ecr-controller") != "--set" {
		t.Errorf("expected image.repository via --set, got args %v", args)
	}
	if setFlagFor(args, "aws.region=eu-central-1") != "--set" {
		t.Errorf("expected aws.region via --set, got args %v", args)
	}

	if !hasArg(args, "--create-namespace") {
		t.Errorf("expected --create-namespace, got %v", args)
	}
}

// TestHelmUpgradeArgs_AlwaysPinsSharedServiceAccount covers the credential
// regression: the chart-created service account has no IRSA annotation and is
// not the account an EKS Pod Identity association is attached to, so a
// controller deployed under it starts with no AWS credentials. Every install
// must therefore disable creation and name the shared account — unconditionally,
// since there is no way to select another.
func TestHelmUpgradeArgs_AlwaysPinsSharedServiceAccount(t *testing.T) {
	args := helmUpgradeArgs(DeployParams{
		ChartDir:  "/charts/ecr",
		Namespace: "ack-system",
		Release:   "ack-ecr-controller",
		ImageRepo: "repo/ecr-controller",
		ImageTag:  "dev",
		Region:    "us-west-2",
	})

	if setFlagFor(args, "serviceAccount.create=false") != "--set" {
		t.Errorf("expected serviceAccount.create=false via --set, got args %v", args)
	}
	// The name goes through --set-string so an all-digit name is not coerced to a
	// number, matching the image.tag handling.
	if got := setFlagFor(args, "serviceAccount.name="+SharedServiceAccount); got != "--set-string" {
		t.Errorf("serviceAccount.name should be passed with --set-string, got %q in %v", got, args)
	}
}

// TestHelmUpgradeArgs_ResyncPeriodUsesSet covers the coercion case that is the
// mirror image of the image tag: the chart's values schema types
// reconcile.defaultResyncPeriod as a number, so it must go through plain --set.
// Passing it with --set-string would make it a string and the schema would
// reject the install.
func TestHelmUpgradeArgs_ResyncPeriodUsesSet(t *testing.T) {
	args := helmUpgradeArgs(DeployParams{
		ChartDir:     "/charts/ecr",
		Namespace:    "ack-system",
		Release:      "ack-ecr-controller",
		ImageRepo:    "repo/ecr-controller",
		ImageTag:     "dev",
		Region:       "us-west-2",
		ResyncPeriod: 60,
	})

	if got := setFlagFor(args, "reconcile.defaultResyncPeriod=60"); got != "--set" {
		t.Errorf("reconcile.defaultResyncPeriod should be passed with --set, got %q in %v", got, args)
	}
}

// TestHelmUpgradeArgs_NoResyncPeriodWhenUnset pins that an unset period leaves
// the chart's own default alone. This is not cosmetic: the chart guards the
// controller flag with `gt (int .Values.reconcile.defaultResyncPeriod) 0`, so
// emitting an explicit 0 would disable periodic resync rather than select the
// default.
func TestHelmUpgradeArgs_NoResyncPeriodWhenUnset(t *testing.T) {
	args := helmUpgradeArgs(DeployParams{
		ChartDir:  "/charts/ecr",
		Namespace: "ack-system",
		Release:   "ack-ecr-controller",
		ImageRepo: "repo/ecr-controller",
		ImageTag:  "dev",
		Region:    "us-west-2",
	})

	for _, a := range args {
		if strings.HasPrefix(a, "reconcile.defaultResyncPeriod") {
			t.Errorf("expected no resync override when ResyncPeriod is 0, got %v", args)
		}
	}
}

// TestExecRegistryImageExists_ClassifiesFailures pins the distinction the reuse
// decision rests on: `aws ecr describe-images` exits non-zero both when the tag
// is genuinely absent and when the lookup itself broke, and collapsing the
// second into "absent" would be harmless (an extra build) while collapsing the
// first into an error would defeat the optimization entirely. Anything that is
// not a recognized not-found signal must surface as an error so the caller
// treats it as unknown.
func TestExecRegistryImageExists_ClassifiesFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		output    string
		wantFound bool
		wantErr   bool
	}{
		{
			name:      "tag absent from an existing repository",
			output:    "An error occurred (ImageNotFoundException) when calling the DescribeImages operation: Requested image not found",
			wantFound: false,
			wantErr:   false,
		},
		{
			// The repository is created later in the same deploy, empty, so a missing
			// repository is a definitive "the tag is not there".
			name:      "repository does not exist yet",
			output:    "An error occurred (RepositoryNotFoundException) when calling the DescribeImages operation: The repository with name 'ecr-controller' does not exist",
			wantFound: false,
			wantErr:   false,
		},
		{
			name:      "expired credentials are inconclusive",
			output:    "An error occurred (ExpiredTokenException) when calling the DescribeImages operation: The security token included in the request is expired",
			wantFound: false,
			wantErr:   true,
		},
		{
			name:      "unrecognized failure is inconclusive",
			output:    "Could not connect to the endpoint URL",
			wantFound: false,
			wantErr:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found, err := classifyImageLookup(tc.output, errFailedLookup)
			if found != tc.wantFound {
				t.Errorf("found = %v, want %v", found, tc.wantFound)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error: %v", err, tc.wantErr)
			}
		})
	}
}
