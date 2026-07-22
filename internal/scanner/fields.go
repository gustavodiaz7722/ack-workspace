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

// resourceSpecSchema returns the OpenAPI schema of a resource's CRD spec,
// resolving the CRD by Kind and parsing down to the spec property. It is the
// shared front half of every field-index builder.
func resourceSpecSchema(repoPath, resource string) (crdSchemaNode, error) {
	crdContent, err := findResourceCRD(repoPath, resource)
	if err != nil {
		return crdSchemaNode{}, err
	}
	var manifest crdManifest
	if err := yaml.Unmarshal([]byte(crdContent), &manifest); err != nil {
		return crdSchemaNode{}, fmt.Errorf("parsing CRD for %q: %w", resource, err)
	}
	if len(manifest.Spec.Versions) == 0 {
		return crdSchemaNode{}, fmt.Errorf("CRD for %q has no versions", resource)
	}
	spec, ok := manifest.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if !ok {
		return crdSchemaNode{}, fmt.Errorf("CRD for %q has no spec schema", resource)
	}
	return spec, nil
}

// specFieldRecords walks a resource's CRD spec into sorted field records with
// the ACK-generated "<name>Ref" companion structures filtered out. It is the
// shared body every field-index builder starts from before applying its own
// per-field markings.
func specFieldRecords(repoPath, resource string) ([]fieldRecord, error) {
	_, records, err := walkedSpecFieldRecords(repoPath, resource)
	return records, err
}

// walkedSpecFieldRecords resolves a resource's CRD spec schema and walks it into
// sorted field records (with the ACK-generated "<name>Ref" companion structures
// filtered out), returning the spec schema alongside the records. Callers that
// need to filter records against the schema shape — for example keeping only
// string-valued fields — use this variant so they do not have to re-resolve and
// re-parse the CRD.
func walkedSpecFieldRecords(repoPath, resource string) (crdSchemaNode, []fieldRecord, error) {
	spec, err := resourceSpecSchema(repoPath, resource)
	if err != nil {
		return crdSchemaNode{}, nil, err
	}
	var records []fieldRecord
	walkFields("", spec, &records)
	records = filterReferenceFields(records)
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	return spec, records, nil
}

// buildFieldIndex produces the combined field index for a resource as a JSON
// document (a one-field-per-line array, so it greps cleanly). Every string-valued
// spec field of the resource's CRD is included with its type, description, dotted
// path, and the is_document / is_iam_policy markings resolved from generator.yaml.
// Object/struct fields and non-string scalars are dropped: a JSON/IAM policy
// document is always carried in a string (or a list of strings), so only
// string-valued fields are candidates. Fields configured as cross-resource
// references are dropped too: they hold an identifier, never a document. Finally,
// fields marked is_primary_key or is_immutable are dropped: a document is never
// the resource's own identifier and is never immutable, so these markings
// reliably identify non-candidates.
func buildFieldIndex(repoPath, resource string) (string, error) {
	spec, records, err := walkedSpecFieldRecords(repoPath, resource)
	if err != nil {
		return "", err
	}
	records = filterNonStringFields(records, stringValuedPaths(spec))
	m, err := loadFieldConfig(repoPath, resource)
	if err != nil {
		return "", err
	}
	records = filterConfiguredReferenceFields(records, m.ref)
	records = filterKeyAndImmutableFields(records, m.immutable, m.primaryKey)
	for i := range records {
		norm := strings.ToLower(records[i].Path)
		records[i].IsDocument = m.doc[norm]
		records[i].IsIAMPolicy = m.iam[norm]
	}
	return marshalFieldIndex(records), nil
}

// filterKeyAndImmutableFields removes fields that generator.yaml marks
// is_primary_key or is_immutable. The document issue uses it because a policy or
// JSON document is never the resource's own identifier and is never immutable —
// document fields are mutable, non-key data — so these markings reliably flag
// non-candidates and dropping them trims the index. matching is case-insensitive
// against the lowercased marking paths.
func filterKeyAndImmutableFields(records []fieldRecord, immutable, primaryKey map[string]bool) []fieldRecord {
	if len(immutable) == 0 && len(primaryKey) == 0 {
		return records
	}
	out := records[:0]
	for _, r := range records {
		norm := strings.ToLower(r.Path)
		if immutable[norm] || primaryKey[norm] {
			continue
		}
		out = append(out, r)
	}
	return out
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

// filterNonStringFields keeps only the records whose path is string-valued
// according to stringPaths (a string leaf, or an array of strings), dropping
// object/struct fields, arrays of objects, and non-string scalars. Both a
// JSON/IAM policy document and a cross-resource reference (an ARN/ID/Name) are
// always carried in a string, so string-valued fields are the only candidates
// for the document and reference issues; dropping the rest shrinks the index and
// removes the struct noise the agent does not care about. Nested string leaves
// are preserved (their object containers are dropped, not the leaves).
func filterNonStringFields(records []fieldRecord, stringPaths map[string]bool) []fieldRecord {
	out := records[:0]
	for _, r := range records {
		if stringPaths[r.Path] {
			out = append(out, r)
		}
	}
	return out
}

// stringValuedPaths returns the set of spec field paths (dotted, camelCase) that
// hold a string value: a string-typed leaf, or an array whose element type is
// string. It walks the same field tree as walkFields so the paths line up with
// the field records, letting filterNonStringFields drop everything else.
func stringValuedPaths(spec crdSchemaNode) map[string]bool {
	paths := map[string]bool{}
	collectStringPaths("", spec, paths)
	return paths
}

// collectStringPaths records into paths every field beneath node whose value is
// a string (a string leaf or an array of strings), recursing into objects and
// array element schemas exactly as walkFields does so the paths match.
func collectStringPaths(path string, node crdSchemaNode, paths map[string]bool) {
	if path != "" && isStringValued(node) {
		paths[path] = true
	}
	if node.Type == "array" && node.Items != nil {
		collectStringChildren(path, *node.Items, paths)
		return
	}
	collectStringChildren(path, node, paths)
}

// collectStringChildren recurses into a node's properties, mirroring
// walkChildren.
func collectStringChildren(path string, node crdSchemaNode, paths map[string]bool) {
	for name, child := range node.Properties {
		collectStringPaths(joinPath(path, name), child, paths)
	}
}

// isStringValued reports whether a node holds a string value: a string-typed
// leaf, or an array whose elements are strings. Objects, arrays of objects, and
// non-string scalars are not string-valued.
func isStringValued(n crdSchemaNode) bool {
	if n.Type == "string" {
		return true
	}
	return n.Type == "array" && n.Items != nil && n.Items.Type == "string"
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
// needs: the document markings, whether the field is immutable or the resource's
// primary key, and whether the field is a cross-resource reference. references is
// captured as a raw node so its mere presence (a non-zero node) signals a
// reference field, without decoding its internals.
type genFieldConfig struct {
	IsDocument   bool      `yaml:"is_document"`
	IsIAMPolicy  bool      `yaml:"is_iam_policy"`
	IsImmutable  bool      `yaml:"is_immutable"`
	IsPrimaryKey bool      `yaml:"is_primary_key"`
	References   yaml.Node `yaml:"references"`
}

// genMarkings decodes only the per-field configuration from generator.yaml.
type genMarkings struct {
	Resources map[string]struct {
		Fields map[string]genFieldConfig `yaml:"fields"`
	} `yaml:"resources"`
}

// fieldMarkings holds the per-field generator.yaml markings the field-index
// builders consult. Each is a set of lowercased field paths, so they correlate
// case-insensitively with the CRD's camelCase paths.
type fieldMarkings struct {
	doc        map[string]bool // is_document
	iam        map[string]bool // is_iam_policy
	ref        map[string]bool // has a references block
	immutable  map[string]bool // is_immutable
	primaryKey map[string]bool // is_primary_key
}

// loadFieldConfig returns the per-field generator.yaml markings for the resource:
// which fields are marked is_document / is_iam_policy, which carry a references
// block, and which are marked is_immutable / is_primary_key. All paths are
// lowercased for case-insensitive correlation with the CRD's camelCase paths.
func loadFieldConfig(repoPath, resource string) (fieldMarkings, error) {
	data, err := os.ReadFile(filepath.Join(repoPath, generatorFileName))
	if err != nil {
		return fieldMarkings{}, fmt.Errorf("reading generator.yaml: %w", err)
	}
	var g genMarkings
	if err := yaml.Unmarshal(data, &g); err != nil {
		return fieldMarkings{}, fmt.Errorf("parsing generator.yaml: %w", err)
	}
	m := fieldMarkings{
		doc:        map[string]bool{},
		iam:        map[string]bool{},
		ref:        map[string]bool{},
		immutable:  map[string]bool{},
		primaryKey: map[string]bool{},
	}
	for path, fc := range g.Resources[resource].Fields {
		norm := strings.ToLower(path)
		if fc.IsDocument {
			m.doc[norm] = true
		}
		if fc.IsIAMPolicy {
			m.iam[norm] = true
		}
		// A non-zero node means a `references:` block is present on the field.
		if !fc.References.IsZero() {
			m.ref[norm] = true
		}
		if fc.IsImmutable {
			m.immutable[norm] = true
		}
		if fc.IsPrimaryKey {
			m.primaryKey[norm] = true
		}
	}
	return m, nil
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
	// IsImmutable reports whether generator.yaml marks the field is_immutable.
	// A reference is frequently immutable (a KMS key, IAM role, parent ID, or
	// subnet is set once and cannot change), so this is a supporting signal for a
	// reference, not a reason to exclude the field.
	IsImmutable bool `json:"is_immutable"`
	// IsPrimaryKey reports whether generator.yaml marks the field is_primary_key.
	// The resource's own primary key is not a cross-resource reference, but a
	// sub-resource's primary key can itself be a reference to its parent, so the
	// model must weigh this together with the field's meaning.
	IsPrimaryKey bool `json:"is_primary_key"`
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
//
// Only string-valued fields are kept: a cross-resource reference holds an
// ARN/ID/Name, always carried in a string (or a list of strings), so object and
// non-string scalar fields are never reference candidates and are dropped.
//
// Unlike the document index, immutable and primary-key fields are NOT dropped: a
// reference is frequently immutable (a KMS key, IAM role, parent ID, or subnet
// is set once) and can even be a sub-resource's primary key, so those markings
// are surfaced on each record as signal rather than used to exclude candidates.
func buildReferenceFieldIndex(repoPath, resource string) (string, error) {
	spec, records, err := walkedSpecFieldRecords(repoPath, resource)
	if err != nil {
		return "", err
	}
	records = filterNonStringFields(records, stringValuedPaths(spec))
	m, err := loadFieldConfig(repoPath, resource)
	if err != nil {
		return "", err
	}
	refRecords := make([]referenceFieldRecord, len(records))
	for i, r := range records {
		norm := strings.ToLower(r.Path)
		refRecords[i] = referenceFieldRecord{
			Path:         r.Path,
			Type:         r.Type,
			Description:  r.Description,
			IsReference:  m.ref[norm] || underReferencePrefix(norm, m.ref),
			IsImmutable:  m.immutable[norm],
			IsPrimaryKey: m.primaryKey[norm],
		}
	}
	return marshalRecordsPerLine(refRecords), nil
}

// fieldTypeRecord is one CRD spec field in the structural field index the
// sub-resource issue greps: just the path, type, and description, with no
// generator.yaml markings. It is deliberately minimal — the issue reasons about
// the shape of the field tree (which fields are nested objects or arrays of
// objects, and whether they carry id/arn/tags sub-fields), not about markings.
type fieldTypeRecord struct {
	Path        string `json:"path"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// buildFieldTypeIndex produces the structural field index for a resource: a
// one-field-per-line JSON array of every spec field with its dotted path, type,
// and description. Unlike the document and reference indexes it carries no
// markings and keeps every field (only the generated "<name>Ref" companions are
// dropped), so the sub-resource issue can see the full nested shape of the CRD.
func buildFieldTypeIndex(repoPath, resource string) (string, error) {
	records, err := specFieldRecords(repoPath, resource)
	if err != nil {
		return "", err
	}
	typeRecords := make([]fieldTypeRecord, len(records))
	for i, r := range records {
		typeRecords[i] = fieldTypeRecord{Path: r.Path, Type: r.Type, Description: r.Description}
	}
	return marshalRecordsPerLine(typeRecords), nil
}
