package scanner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws-controllers-k8s/pkg/names"
	"gopkg.in/yaml.v3"
)

// The Smithy model is the only place a nested CRD field's documentation exists.
// ACK's code-generator propagates a field's description into the CRD for
// top-level spec fields but not for nested ones, so a nested field arrives in
// the field index as a bare {path, type} record with nothing for the agent to
// reason about. This file rebuilds that missing documentation by walking the
// service's Smithy model the same way the code-generator does and joining the
// result onto CRD field paths.
//
// The join is exact rather than heuristic: the code-generator derives a CRD
// property name from a model member with names.New(member).CamelLower (see
// code-generator/pkg/model/field.go, which uses Names.CamelLower as the field's
// JSON tag), so importing that same package reproduces the paths instead of
// approximating them.

// maxShapeDepth bounds the model walk. Smithy shape graphs are recursive (a
// shape can transitively contain itself), and the visited-set below breaks true
// cycles, but a depth ceiling also stops the walk from enumerating pathologically
// deep structures that the CRD does not contain either.
const maxShapeDepth = 8

// smithyDoc is what the model knows about one field: its documentation and, when
// the member's target shape constrains it, the validation pattern. The pattern is
// the strongest available reference signal because an ARN pattern names the
// target service and resource type outright (for example
// "^arn:aws[a-z\-]*:iam::\d{12}:role/?...$").
type smithyDoc struct {
	Description string
	Pattern     string
}

// smithyModel is the subset of a Smithy JSON model this package decodes.
type smithyModel struct {
	Shapes map[string]smithyShape `json:"shapes"`
}

// smithyShape is one shape: its type, its members (structures and unions), its
// element shape (lists), and its traits.
type smithyShape struct {
	Type    string                     `json:"type"`
	Members map[string]smithyMember    `json:"members"`
	Member  *smithyMember              `json:"member"`
	Traits  map[string]json.RawMessage `json:"traits"`
}

// smithyMember is one member of a structure or union shape.
type smithyMember struct {
	Target string                     `json:"target"`
	Traits map[string]json.RawMessage `json:"traits"`
}

// docIndex is the resolved model documentation for one resource. It has two
// layers with different precision, consulted in order by lookup:
//
//   - byPath: documentation reached by structurally walking the resource's
//     operation input shapes. Precise, because the path identifies exactly which
//     shape's member the documentation came from.
//   - byMember: documentation keyed by member name alone, used only for fields
//     the structural walk did not reach (for example rds's Parameter struct,
//     which hangs off a Describe output rather than the Create input). It is
//     restricted to member names carrying a single distinct documentation string
//     across the whole model; 39.6% of member names in the models measured are
//     ambiguous (Description appears with 99 different meanings, State with 112),
//     and attaching an arbitrary one of those to a field would mislead the agent
//     more than leaving the description empty.
type docIndex struct {
	byPath   map[string]smithyDoc
	byMember map[string]smithyDoc
	// lowerPath maps a lowercased path to its canonical form so a join can
	// tolerate initialism skew between the names table this binary was built
	// against and the one that generated the controller (for example a CRD's
	// "argoCD" against a freshly computed "argoCd"). Paths that collide when
	// lowercased are excluded, so an ambiguous match never resolves.
	lowerPath map[string]string
}

// lookup returns the model's documentation for a CRD field path. It prefers the
// structural match, falls back to a case-insensitive structural match, and only
// then to the unambiguous member-name index.
func (d docIndex) lookup(path string) (smithyDoc, bool) {
	if doc, ok := d.byPath[path]; ok {
		return doc, true
	}
	if canonical, ok := d.lowerPath[strings.ToLower(path)]; ok {
		if doc, ok := d.byPath[canonical]; ok {
			return doc, true
		}
	}
	leaf := path
	if i := strings.LastIndex(leaf, "."); i >= 0 {
		leaf = leaf[i+1:]
	}
	doc, ok := d.byMember[leaf]
	return doc, ok
}

// buildDocIndex resolves the model documentation for one resource of one
// controller: it decodes the model, determines which operation input shapes feed
// the resource's spec, walks them into CRD-shaped field paths, and builds the
// unambiguous member-name fallback.
func buildDocIndex(modelJSON, repoPath, resource string) (docIndex, error) {
	var m smithyModel
	if err := json.Unmarshal([]byte(modelJSON), &m); err != nil {
		return docIndex{}, fmt.Errorf("parsing smithy model: %w", err)
	}
	if len(m.Shapes) == 0 {
		return docIndex{}, fmt.Errorf("smithy model declares no shapes")
	}

	cfg, err := loadSpecSources(repoPath, resource)
	if err != nil {
		return docIndex{}, err
	}

	idx := docIndex{
		byPath:    map[string]smithyDoc{},
		byMember:  unambiguousMemberDocs(m),
		lowerPath: map[string]string{},
	}
	for _, root := range m.rootShapes(resource, cfg.operations) {
		m.walkShape(root, "", 0, map[string]bool{}, cfg.renames, idx.byPath)
	}

	// Index paths by their lowercased form, dropping any that collide so a
	// case-insensitive join can never pick between two candidates.
	collisions := map[string]bool{}
	for path := range idx.byPath {
		lower := strings.ToLower(path)
		if _, seen := idx.lowerPath[lower]; seen {
			collisions[lower] = true
			continue
		}
		idx.lowerPath[lower] = path
	}
	for lower := range collisions {
		delete(idx.lowerPath, lower)
	}
	return idx, nil
}

// rootShapes returns the structure shapes whose members become the resource's
// spec fields: the resource's own Create operation input, plus the input of every
// operation a generator.yaml custom field sources from.
//
// The second group matters more than its size suggests. ACK synthesizes spec
// fields out of unrelated operations — lambda's Alias resource carries
// permissions.* from AddPermission and functionEventInvokeConfig.* from
// PutFunctionEventInvokeConfig — and those synthesized subtrees are entirely
// nested, so they are exactly the fields with no CRD description.
func (m smithyModel) rootShapes(resource string, operations []string) []string {
	wanted := map[string]bool{}
	for _, suffix := range []string{"Request", "Input", "Message"} {
		wanted[strings.ToLower("Create"+resource+suffix)] = true
	}
	for _, op := range operations {
		for _, suffix := range []string{"Request", "Input", "Message"} {
			wanted[strings.ToLower(op+suffix)] = true
		}
	}

	var roots []string
	for name, shape := range m.Shapes {
		if shape.Type != "structure" && shape.Type != "union" {
			continue
		}
		if wanted[strings.ToLower(shortShapeName(name))] {
			roots = append(roots, name)
		}
	}
	return roots
}

// walkShape descends a structure or union shape, recording one entry per member
// keyed by the dotted CRD path. Members are named with
// names.New(member).CamelLower, the transform the code-generator uses for the
// field's JSON tag, and list shapes are unwrapped onto the same path because ACK
// flattens an array's element fields rather than indexing them.
func (m smithyModel) walkShape(
	shapeName, path string,
	depth int,
	visiting map[string]bool,
	renames map[string]string,
	out map[string]smithyDoc,
) {
	if depth > maxShapeDepth {
		return
	}
	shapeName, shape, ok := m.resolveStruct(shapeName)
	if !ok || visiting[shapeName] {
		return
	}
	visiting[shapeName] = true
	defer delete(visiting, shapeName)

	for memberName, member := range shape.Members {
		// generator.yaml renames apply to an operation's input fields, so only
		// the top level of a root shape is subject to them.
		effective := memberName
		if path == "" {
			if renamed, ok := renames[memberName]; ok {
				effective = renamed
			}
		}
		fieldPath := names.New(effective).CamelLower
		if path != "" {
			fieldPath = path + "." + fieldPath
		}

		doc := smithyDoc{
			Description: traitDoc(member.Traits),
			Pattern:     m.targetPattern(member.Target),
		}
		if doc.Description == "" {
			// A member without its own documentation is effectively documented by
			// the shape it targets, which is where AWS puts the description for
			// shared value types.
			if target, ok := m.Shapes[member.Target]; ok {
				doc.Description = traitDoc(target.Traits)
			}
		}
		// Several root shapes can contribute the same path (Create and a custom
		// field's source operation often share members). Prefer whichever
		// actually carries documentation.
		if existing, seen := out[fieldPath]; !seen || (existing.Description == "" && doc.Description != "") {
			out[fieldPath] = doc
		}

		m.walkShape(member.Target, fieldPath, depth+1, visiting, renames, out)
	}
}

// resolveStruct follows list shapes to their element shape and reports the
// resulting structure or union. A non-aggregate shape (a string, an enum, a map)
// has no members to walk and yields false.
func (m smithyModel) resolveStruct(shapeName string) (string, smithyShape, bool) {
	for i := 0; i <= maxShapeDepth; i++ {
		shape, ok := m.Shapes[shapeName]
		if !ok {
			return "", smithyShape{}, false
		}
		if shape.Type == "list" && shape.Member != nil {
			shapeName = shape.Member.Target
			continue
		}
		if shape.Type == "structure" || shape.Type == "union" {
			return shapeName, shape, true
		}
		return "", smithyShape{}, false
	}
	return "", smithyShape{}, false
}

// targetPattern returns the smithy.api#pattern constraint on a member's target
// shape, if any. For an identifier field this is often an ARN template that names
// the referenced service and resource type explicitly, which is the single
// strongest signal that a field is a cross-resource reference.
func (m smithyModel) targetPattern(target string) string {
	shape, ok := m.Shapes[target]
	if !ok {
		return ""
	}
	raw, ok := shape.Traits["smithy.api#pattern"]
	if !ok {
		return ""
	}
	var pattern string
	if json.Unmarshal(raw, &pattern) != nil {
		return ""
	}
	return pattern
}

// unambiguousMemberDocs indexes documentation by member name, keeping only names
// that mean the same thing everywhere they appear. A member name carrying more
// than one distinct documentation string across the model is dropped: the index
// is a fallback for fields the structural walk missed, and a wrong description is
// worse than none.
func unambiguousMemberDocs(m smithyModel) map[string]smithyDoc {
	type candidate struct {
		doc      smithyDoc
		distinct map[string]bool
	}
	seen := map[string]*candidate{}
	for _, shape := range m.Shapes {
		for memberName, member := range shape.Members {
			doc := traitDoc(member.Traits)
			if doc == "" {
				continue
			}
			key := names.New(memberName).CamelLower
			c, ok := seen[key]
			if !ok {
				c = &candidate{distinct: map[string]bool{}}
				seen[key] = c
			}
			c.distinct[doc] = true
			if c.doc.Description == "" {
				c.doc = smithyDoc{Description: doc, Pattern: m.targetPattern(member.Target)}
			}
		}
	}

	out := make(map[string]smithyDoc, len(seen))
	for key, c := range seen {
		if len(c.distinct) == 1 {
			out[key] = c.doc
		}
	}
	return out
}

// traitDoc extracts the smithy.api#documentation trait as text.
func traitDoc(traits map[string]json.RawMessage) string {
	raw, ok := traits["smithy.api#documentation"]
	if !ok {
		return ""
	}
	var doc string
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return strings.TrimSpace(doc)
}

// specSources is the generator.yaml configuration that affects how model members
// map onto CRD field paths for one resource.
type specSources struct {
	// renames maps an original operation input field name to the name ACK
	// exposes (generator.yaml renames.operations.<Op>.input_fields).
	renames map[string]string
	// operations are the additional operations whose input shapes contribute spec
	// fields, declared by custom fields as `from: {operation: ...}`.
	operations []string
}

// generatorSpecSources decodes the two parts of generator.yaml that determine
// which model shapes and member names a resource's spec is built from.
type generatorSpecSources struct {
	Resources map[string]struct {
		Fields map[string]struct {
			From *struct {
				Operation string `yaml:"operation"`
			} `yaml:"from"`
		} `yaml:"fields"`
		Renames *struct {
			Operations map[string]struct {
				InputFields map[string]string `yaml:"input_fields"`
			} `yaml:"operations"`
		} `yaml:"renames"`
	} `yaml:"resources"`
}

// loadSpecSources reads the resource's renames and custom-field source operations
// from generator.yaml. Both are optional; a resource that configures neither
// yields empty values and the walk proceeds from the Create input shape alone.
func loadSpecSources(repoPath, resource string) (specSources, error) {
	data, err := os.ReadFile(filepath.Join(repoPath, generatorFileName))
	if err != nil {
		return specSources{}, fmt.Errorf("reading generator.yaml: %w", err)
	}
	var g generatorSpecSources
	if err := yaml.Unmarshal(data, &g); err != nil {
		return specSources{}, fmt.Errorf("parsing generator.yaml: %w", err)
	}

	out := specSources{renames: map[string]string{}}
	rc, ok := g.Resources[resource]
	if !ok {
		return out, nil
	}
	if rc.Renames != nil {
		for _, op := range rc.Renames.Operations {
			for original, renamed := range op.InputFields {
				out.renames[original] = renamed
			}
		}
	}
	seen := map[string]bool{}
	for _, fc := range rc.Fields {
		if fc.From == nil || fc.From.Operation == "" || seen[fc.From.Operation] {
			continue
		}
		seen[fc.From.Operation] = true
		out.operations = append(out.operations, fc.From.Operation)
	}
	return out, nil
}
