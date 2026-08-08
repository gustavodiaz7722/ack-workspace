package candidates

import (
	"testing"
)

func TestStringValuedPaths(t *testing.T) {
	spec := crdSchemaNode{
		Type: "object",
		Properties: map[string]crdSchemaNode{
			"name":    {Type: "string"},
			"enabled": {Type: "boolean"},
			"count":   {Type: "integer"},
			"aliases": {Type: "array", Items: &crdSchemaNode{Type: "string"}},
			"config": {Type: "object", Properties: map[string]crdSchemaNode{
				"policy": {Type: "string"},
			}},
			"rules": {Type: "array", Items: &crdSchemaNode{Type: "object", Properties: map[string]crdSchemaNode{
				"target": {Type: "string"},
			}}},
		},
	}

	paths := stringValuedPaths(spec)

	// String leaves, arrays of strings, and nested string leaves are string-valued.
	for _, want := range []string{"name", "aliases", "config.policy", "rules.target"} {
		if !paths[want] {
			t.Errorf("expected %q to be string-valued; got %v", want, paths)
		}
	}
	// Non-string scalars, object containers, and arrays of objects are not.
	for _, notWant := range []string{"enabled", "count", "config", "rules"} {
		if paths[notWant] {
			t.Errorf("%q should not be string-valued; got %v", notWant, paths)
		}
	}
}

func TestFilterNonStringFields(t *testing.T) {
	in := []fieldRecord{
		{Path: "name", Type: "string"},
		{Path: "config", Type: "object"},
		{Path: "config.policy", Type: "string"},
		{Path: "count", Type: "integer"},
		{Path: "aliases", Type: "array"},
	}
	stringPaths := map[string]bool{"name": true, "config.policy": true, "aliases": true}

	got := map[string]bool{}
	for _, r := range filterNonStringFields(in, stringPaths) {
		got[r.Path] = true
	}
	if !got["name"] || !got["config.policy"] || !got["aliases"] {
		t.Errorf("string-valued fields were dropped: %v", got)
	}
	for _, dropped := range []string{"config", "count"} {
		if got[dropped] {
			t.Errorf("non-string field %q should have been filtered", dropped)
		}
	}
}

func TestFilterReferenceFields(t *testing.T) {
	in := []fieldRecord{
		{Path: "deliveryPolicy"},
		{Path: "roleRef"},
		{Path: "roleRef.from"},
		{Path: "roleRef.from.name"},
		{Path: "roleRef.from.namespace"},
		{Path: "securityGroupRefs"},
		{Path: "securityGroupRefs.from.name"},
		{Path: "notAReference"}, // ends in nothing special; kept
	}
	got := map[string]bool{}
	for _, r := range filterReferenceFields(in) {
		got[r.Path] = true
	}
	if !got["deliveryPolicy"] || !got["notAReference"] {
		t.Errorf("non-reference fields were dropped: %v", got)
	}
	for _, dropped := range []string{"roleRef", "roleRef.from", "roleRef.from.name", "securityGroupRefs", "securityGroupRefs.from.name"} {
		if got[dropped] {
			t.Errorf("reference field %q should have been filtered", dropped)
		}
	}
}
