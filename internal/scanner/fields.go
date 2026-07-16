package scanner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// fieldRecord is one CRD spec field in the combined field index the model
// greps. It fuses the CRD's structural information (path, type, description)
// with the generator.yaml markings (is_document, is_iam_policy) so the model can
// see, in one place, every candidate field and whether it is already marked.
type fieldRecord struct {
	// Path is the field's dotted path within the resource spec, in the CRD's
	// (camelCase) naming, for example "domainValidationOptions.validationDomain".
	// Array element fields use the parent path without an index.
	Path string `json:"path"`
	// Type is the OpenAPI type of the field (string, object, array, boolean, …).
	Type string `json:"type"`
	// Description is the field's CRD description, if any.
	Description string `json:"description,omitempty"`
	// IsDocument reports whether generator.yaml marks the field is_document.
	IsDocument bool `json:"is_document"`
	// IsIAMPolicy reports whether generator.yaml marks the field is_iam_policy.
	IsIAMPolicy bool `json:"is_iam_policy"`
}

// crdSchemaNode is the recursive subset of an OpenAPI v3 schema needed to walk a
// CRD's field tree.
type crdSchemaNode struct {
	Type        string                   `yaml:"type"`
	Description string                   `yaml:"description"`
	Properties  map[string]crdSchemaNode `yaml:"properties"`
	Items       *crdSchemaNode           `yaml:"items"`
}

// crdManifest is the subset of a CRD manifest needed to reach the spec schema.
type crdManifest struct {
	Spec struct {
		Versions []struct {
			Schema struct {
				OpenAPIV3Schema crdSchemaNode `yaml:"openAPIV3Schema"`
			} `yaml:"schema"`
		} `yaml:"versions"`
	} `yaml:"spec"`
}

// buildFieldIndex produces the combined field index for a resource as a JSON
// document (a one-field-per-line array, so it greps cleanly). Every spec field
// of the resource's CRD is included with its type, description, dotted path, and
// the is_document / is_iam_policy markings resolved from generator.yaml.
func buildFieldIndex(repoPath, resource string) (string, error) {
	crdContent, err := findResourceCRD(repoPath, resource)
	if err != nil {
		return "", err
	}
	var manifest crdManifest
	if err := yaml.Unmarshal([]byte(crdContent), &manifest); err != nil {
		return "", fmt.Errorf("parsing CRD for %q: %w", resource, err)
	}
	if len(manifest.Spec.Versions) == 0 {
		return "", fmt.Errorf("CRD for %q has no versions", resource)
	}
	spec, ok := manifest.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if !ok {
		return "", fmt.Errorf("CRD for %q has no spec schema", resource)
	}

	docPaths, iamPaths, refPaths, err := loadFieldConfig(repoPath, resource)
	if err != nil {
		return "", err
	}

	var records []fieldRecord
	walkFields("", spec, &records)
	records = filterReferenceFields(records)
	records = filterConfiguredReferenceFields(records, refPaths)
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	for i := range records {
		norm := strings.ToLower(records[i].Path)
		records[i].IsDocument = docPaths[norm]
		records[i].IsIAMPolicy = iamPaths[norm]
	}
	return marshalFieldIndex(records), nil
}

// walkFields appends a fieldRecord for every property beneath node (recursively)
// to out. path is the dotted path to node ("" for the spec root, which is not
// itself emitted). Array element properties extend the parent path without an
// index segment, so a field names the attribute rather than a position.
func walkFields(path string, node crdSchemaNode, out *[]fieldRecord) {
	if path != "" {
		*out = append(*out, fieldRecord{Path: path, Type: nodeType(node), Description: node.Description})
	}
	// Descend: into an array's element schema (same path) or an object's
	// properties (extended path).
	if node.Type == "array" && node.Items != nil {
		walkChildren(path, *node.Items, out)
		return
	}
	walkChildren(path, node, out)
}

// walkChildren recurses into a node's properties.
func walkChildren(path string, node crdSchemaNode, out *[]fieldRecord) {
	for name, child := range node.Properties {
		walkFields(joinPath(path, name), child, out)
	}
}

// nodeType returns a node's declared type, inferring "object" when a typeless
// node has properties.
func nodeType(n crdSchemaNode) string {
	if n.Type != "" {
		return n.Type
	}
	if len(n.Properties) > 0 {
		return "object"
	}
	return ""
}

// joinPath extends a dotted path with the next segment.
func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// filterReferenceFields removes ACK-generated cross-resource reference fields
// from the index. For each configured reference the code-generator emits a
// "<name>Ref"/"<name>Refs" field with a "from.name" (and optional
// "from.namespace") sub-structure; none of these are user-facing fields that
// need document annotation.
//
// It is evidence-based to avoid false positives on a field that merely ends in
// "Ref": a "<name>Ref" segment is treated as a reference only when a
// "from.name"/"from.namespace" child actually exists. When it does, the entire
// reference subtree is dropped — the container, its "from" object, and the
// leaves — not just the leaves.
func filterReferenceFields(records []fieldRecord) []fieldRecord {
	// Pass 1: collect the path prefixes that are confirmed reference containers.
	refPrefixes := map[string]bool{}
	for _, r := range records {
		parts := strings.Split(r.Path, ".")
		for i := 0; i+2 < len(parts); i++ {
			seg := parts[i]
			if (strings.HasSuffix(seg, "Ref") || strings.HasSuffix(seg, "Refs")) &&
				parts[i+1] == "from" && (parts[i+2] == "name" || parts[i+2] == "namespace") {
				refPrefixes[strings.Join(parts[:i+1], ".")] = true
			}
		}
	}
	if len(refPrefixes) == 0 {
		return records
	}

	// Pass 2: drop every field at or beneath a reference container.
	out := records[:0]
	for _, r := range records {
		if !underReferencePrefix(r.Path, refPrefixes) {
			out = append(out, r)
		}
	}
	return out
}

// underReferencePrefix reports whether path is a reference container or nested
// beneath one.
func underReferencePrefix(path string, refPrefixes map[string]bool) bool {
	for prefix := range refPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+".") {
			return true
		}
	}
	return false
}

// filterConfiguredReferenceFields removes fields that generator.yaml configures
// as cross-resource references. A reference field holds an ARN or identifier
// that points at another resource, so it can never hold a JSON or IAM policy
// document; the two sets are mutually exclusive. Dropping reference fields
// shrinks the index the document issue must search without discarding any
// candidate.
//
// This complements filterReferenceFields, which removes the generated
// "<name>Ref" companion structures found in the CRD; this removes the reference
// value field itself (for example the ARN string), which those structures shadow
// but do not eliminate. refPaths holds the lowercased reference field paths from
// generator.yaml, matched case-insensitively against the CRD paths (camelCase).
func filterConfiguredReferenceFields(records []fieldRecord, refPaths map[string]bool) []fieldRecord {
	if len(refPaths) == 0 {
		return records
	}
	out := records[:0]
	for _, r := range records {
		norm := strings.ToLower(r.Path)
		if refPaths[norm] || underReferencePrefix(norm, refPaths) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// genFieldConfig decodes only the per-field configuration the field index
// needs: the document markings, and whether the field is a cross-resource
// reference. references is captured as a raw node so its mere presence (a
// non-zero node) signals a reference field, without decoding its internals.
type genFieldConfig struct {
	IsDocument  bool      `yaml:"is_document"`
	IsIAMPolicy bool      `yaml:"is_iam_policy"`
	References  yaml.Node `yaml:"references"`
}

// genMarkings decodes only the per-field configuration from generator.yaml.
type genMarkings struct {
	Resources map[string]struct {
		Fields map[string]genFieldConfig `yaml:"fields"`
	} `yaml:"resources"`
}

// loadFieldConfig returns the sets of field paths (lowercased for
// case-insensitive correlation with CRD camelCase paths) that generator.yaml
// marks as is_document or is_iam_policy, plus the set of field paths configured
// as cross-resource references, for the resource.
func loadFieldConfig(repoPath, resource string) (doc, iam, ref map[string]bool, err error) {
	data, err := os.ReadFile(filepath.Join(repoPath, generatorFileName))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reading generator.yaml: %w", err)
	}
	var g genMarkings
	if err := yaml.Unmarshal(data, &g); err != nil {
		return nil, nil, nil, fmt.Errorf("parsing generator.yaml: %w", err)
	}
	doc, iam, ref = map[string]bool{}, map[string]bool{}, map[string]bool{}
	for path, fc := range g.Resources[resource].Fields {
		norm := strings.ToLower(path)
		if fc.IsDocument {
			doc[norm] = true
		}
		if fc.IsIAMPolicy {
			iam[norm] = true
		}
		// A non-zero node means a `references:` block is present on the field.
		if !fc.References.IsZero() {
			ref[norm] = true
		}
	}
	return doc, iam, ref, nil
}

// marshalFieldIndex renders the records as a JSON array with one field object
// per line, so grep returns whole field records rather than fragments.
func marshalFieldIndex(records []fieldRecord) string {
	return marshalRecordsPerLine(records)
}

// marshalRecordsPerLine renders a slice of records as a JSON array with one
// record object per line, so grep returns whole records rather than fragments.
func marshalRecordsPerLine[T any](records []T) string {
	if len(records) == 0 {
		return "[]\n"
	}
	var b strings.Builder
	b.WriteString("[\n")
	for i, r := range records {
		line, _ := json.Marshal(r)
		b.Write(line)
		if i < len(records)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("]\n")
	return b.String()
}

// referenceFieldRecord is one CRD spec field in the field index the reference
// issue greps. Unlike the document issue's index it does not carry document
// markings; instead it records whether generator.yaml already configures the
// field as a cross-resource reference, so the model can tell a field that is
// correctly wired from one that is a candidate missing its reference.
type referenceFieldRecord struct {
	// Path is the field's dotted path within the resource spec, in the CRD's
	// (camelCase) naming, for example "lambdaConfig.preSignUp".
	Path string `json:"path"`
	// Type is the OpenAPI type of the field (string, object, array, …).
	Type string `json:"type"`
	// Description is the field's CRD description, if any.
	Description string `json:"description,omitempty"`
	// IsReference reports whether generator.yaml already configures the field
	// with a references block.
	IsReference bool `json:"is_reference"`
}

// buildReferenceFieldIndex produces the field index the reference issue greps: a
// one-field-per-line JSON array of every spec field of the resource's CRD, each
// marked with whether generator.yaml already configures it as a cross-resource
// reference.
//
// Unlike the document issue's index (buildFieldIndex), reference-configured
// fields are kept — the point of this issue is to check whether reference fields
// are configured, so they must be present and flagged. The generated "<name>Ref"
// companion structures are still dropped (via filterReferenceFields) because
// they are ACK plumbing, not API fields.
func buildReferenceFieldIndex(repoPath, resource string) (string, error) {
	crdContent, err := findResourceCRD(repoPath, resource)
	if err != nil {
		return "", err
	}
	var manifest crdManifest
	if err := yaml.Unmarshal([]byte(crdContent), &manifest); err != nil {
		return "", fmt.Errorf("parsing CRD for %q: %w", resource, err)
	}
	if len(manifest.Spec.Versions) == 0 {
		return "", fmt.Errorf("CRD for %q has no versions", resource)
	}
	spec, ok := manifest.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if !ok {
		return "", fmt.Errorf("CRD for %q has no spec schema", resource)
	}

	_, _, refPaths, err := loadFieldConfig(repoPath, resource)
	if err != nil {
		return "", err
	}

	var records []fieldRecord
	walkFields("", spec, &records)
	records = filterReferenceFields(records)
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })

	refRecords := make([]referenceFieldRecord, len(records))
	for i, r := range records {
		norm := strings.ToLower(r.Path)
		refRecords[i] = referenceFieldRecord{
			Path:        r.Path,
			Type:        r.Type,
			Description: r.Description,
			IsReference: refPaths[norm] || underReferencePrefix(norm, refPaths),
		}
	}
	return marshalRecordsPerLine(refRecords), nil
}
