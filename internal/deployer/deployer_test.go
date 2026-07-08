// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
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

func TestHelmUpgradeArgs_TagUsesSetString(t *testing.T) {
	// An all-digit commit SHA is the regression case: with plain --set, Helm
	// coerces it to a number and the chart's values schema rejects it with
	// "got number, want string".
	const tag = "4881291"
	args := helmUpgradeArgs(
		"/charts/ecr",
		"ack-system",
		"ack-ecr-controller",
		"123456789012.dkr.ecr.us-west-2.amazonaws.com/ecr-controller",
		tag,
		"us-west-2",
	)

	if got := setFlagFor(args, "image.tag="+tag); got != "--set-string" {
		t.Errorf("image.tag should be passed with --set-string, got %q", got)
	}

	// The tag value must be preserved verbatim.
	if _, ok := argAfter(args, "--set-string"); !ok {
		t.Fatalf("expected a --set-string flag with a following value in %v", args)
	}
}

func TestHelmUpgradeArgs_CoreArgs(t *testing.T) {
	args := helmUpgradeArgs(
		"/charts/ecr",
		"ack-test",
		"ack-ecr-controller",
		"repo/ecr-controller",
		"dev",
		"eu-central-1",
	)

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

	found := false
	for _, a := range args {
		if a == "--create-namespace" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected --create-namespace, got %v", args)
	}
}
