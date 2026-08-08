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

import "testing"

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

// TestHelmUpgradeArgs_NoServiceAccountWhenUnset pins that the argument builder
// only emits service-account overrides when it is given one, leaving the
// chart's own handling alone otherwise. A deploy always names an account (the
// one the cluster binds credentials to), so this covers the builder in
// isolation rather than a reachable deploy path.
func TestHelmUpgradeArgs_NoServiceAccountWhenUnset(t *testing.T) {
	args := helmUpgradeArgs(DeployParams{
		ChartDir:  "/charts/ecr",
		Namespace: "ack-system",
		Release:   "ack-ecr-controller",
		ImageRepo: "repo/ecr-controller",
		ImageTag:  "dev",
		Region:    "us-west-2",
	})

	for _, a := range args {
		if a == "serviceAccount.create=false" || len(a) > 19 && a[:19] == "serviceAccount.name" {
			t.Errorf("expected no serviceAccount overrides when ServiceAccount is empty, got %v", args)
		}
	}
}

// TestHelmUpgradeArgs_ServiceAccountReusesExisting covers the credential
// regression: the chart-created service account has no IRSA annotation and is
// not the account an EKS Pod Identity association is attached to, so a
// controller deployed under it starts with no AWS credentials. Naming an
// existing account must both disable creation and reference that name.
func TestHelmUpgradeArgs_ServiceAccountReusesExisting(t *testing.T) {
	const sa = "ack-controller"
	args := helmUpgradeArgs(DeployParams{
		ChartDir:       "/charts/ecr",
		Namespace:      "ack-system",
		Release:        "ack-ecr-controller",
		ImageRepo:      "repo/ecr-controller",
		ImageTag:       "dev",
		Region:         "us-west-2",
		ServiceAccount: sa,
	})

	if setFlagFor(args, "serviceAccount.create=false") != "--set" {
		t.Errorf("expected serviceAccount.create=false via --set, got args %v", args)
	}
	// The name goes through --set-string so an all-digit name is not coerced to a
	// number, matching the image.tag handling.
	if got := setFlagFor(args, "serviceAccount.name="+sa); got != "--set-string" {
		t.Errorf("serviceAccount.name should be passed with --set-string, got %q in %v", got, args)
	}
}
