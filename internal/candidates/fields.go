package candidates

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// fieldRecord is one CRD spec field as walked out of the resource's CRD: its
// dotted path, OpenAPI type, and description. It is the structural half of a
// candidate record, which the Indexer fuses with the generator.yaml markings
// and the API model's documentation.
type fieldRecord struct {
	// Path is the field's dotted path within the resource spec, in the CRD's
	// (camelCase) naming, for example "domainValidationOptions.validationDomain".
	// Array element fields use the parent path without an index.
	Path string
	// Type is the OpenAPI type of the field (string, object, array, boolean, …).
	Type string
	// Description is the field's CRD description, if any.
	Description string
}

// crdSchemaNode is the recursive subset of an OpenAPI v3 schema needed to walk
// a CRD's field tree.
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

// walkedSpecFieldRecords resolves a resource's CRD spec schema and walks it
// into sorted field records (with the ACK-generated "<name>Ref" companion
// structures filtered out), returning the spec schema alongside the records.
// Callers that need to filter records against the schema shape — for example
// keeping only string-valued fields — use this variant so they do not have to
// re-resolve and re-parse the CRD.
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

// walkFields appends a fieldRecord for every property beneath node
// (recursively) to out. path is the dotted path to node ("" for the spec root,
// which is not itself emitted). Array element properties extend the parent path
// without an index segment, so a field names the attribute rather than a
// position.
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
// cross-resource reference holds an ARN, ID, or Name, always carried in a
// string, so string-valued fields are the only candidates and dropping the rest
// removes pure structural noise. Nested string leaves are preserved; only their
// object containers are dropped.
func filterNonStringFields(records []fieldRecord, stringPaths map[string]bool) []fieldRecord {
	out := records[:0]
	for _, r := range records {
		if stringPaths[r.Path] {
			out = append(out, r)
		}
	}
	return out
}

// stringValuedPaths returns the set of spec field paths (dotted, camelCase)
// that hold a string value: a string-typed leaf, or an array whose element type
// is string. It walks the same field tree as walkFields so the paths line up
// with the field records, letting filterNonStringFields drop everything else.
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

// genFieldConfig decodes only the per-field configuration the candidate index
// needs: whether the field is immutable or the resource's primary key, and
// whether it is already a cross-resource reference. references is captured as a
// raw node so its mere presence (a non-zero node) signals a reference field,
// without decoding its internals.
type genFieldConfig struct {
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
	ref        map[string]bool // has a references block
	immutable  map[string]bool // is_immutable
	primaryKey map[string]bool // is_primary_key
}

// loadFieldConfig returns the per-field generator.yaml markings for the
// resource: which fields carry a references block, and which are marked
// is_immutable / is_primary_key. All paths are lowercased for case-insensitive
// correlation with the CRD's camelCase paths.
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
		ref:        map[string]bool{},
		immutable:  map[string]bool{},
		primaryKey: map[string]bool{},
	}
	for path, fc := range g.Resources[resource].Fields {
		norm := strings.ToLower(path)
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
