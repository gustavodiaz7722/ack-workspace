package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// CandidateRecord is one cross-resource-reference candidate field: a
// string-valued spec field of a resource's CRD, fused with the generator.yaml
// markings that bear on whether it is a reference and with the API model's
// documentation and validation pattern.
//
// The record is the unit of the candidate index, emitted one per line as JSON so
// it greps cleanly and so a reviewer can diff two runs. Only string-valued fields
// appear: a reference holds an ARN, ID, or Name, always carried in a string or a
// list of strings, so object and non-string scalar fields are never candidates.
// The ACK-generated "<name>Ref" companion structures are dropped as well —
// they are controller plumbing, not API fields.
type CandidateRecord struct {
	// Resource is the resource Kind the field belongs to. It is carried on every
	// record so a merged multi-resource stream stays interpretable.
	Resource string `json:"resource"`
	// Path is the CRD field path in dot notation and camelCase, matching what a
	// user writes in a manifest (for example "lambdaConfig.preSignUp").
	Path string `json:"path"`
	// Type is the field's CRD type: "string", or "array" for a list of strings.
	Type string `json:"type"`
	// Description is what the field holds. ACK propagates descriptions into the
	// CRD only for top-level spec fields, so for a nested field this is resolved
	// from the service's Smithy model instead.
	Description string `json:"description,omitempty"`
	// DescriptionSource is "crd" or "model", so a reader can weigh the
	// description and tell a missing one from an empty one.
	DescriptionSource string `json:"description_source,omitempty"`
	// ModelJoin records how a model-sourced description and pattern were matched
	// to this field: "path" when the model's shape graph was walked to this exact
	// field path, "member" when the structural walk did not reach the field and
	// the value came from the model-wide member-name fallback. It is empty when
	// the model supplied nothing.
	//
	// A "path" join cannot attribute an unrelated shape's documentation to a
	// field. A "member" join is restricted to member names that mean the same
	// thing everywhere in the model, so it is never arbitrary, but it is the
	// weaker of the two and worth noticing on a field a finding rests on.
	ModelJoin string `json:"model_join,omitempty"`
	// Pattern is the validation pattern the API model constrains the field with.
	// For an identifier this is frequently an ARN template naming the referenced
	// service and resource type outright, the strongest available signal that the
	// field is a cross-resource reference.
	Pattern string `json:"pattern,omitempty"`
	// IsReference reports whether generator.yaml already configures the field
	// with a references block. This is the authoritative answer to "is this
	// already done"; the CRD's companion fields are not, because sibling fields
	// collapse onto one companion name.
	IsReference bool `json:"is_reference"`
	// ReferenceTarget is the configured target, rendered as "<service> <Kind> ->
	// <path>" (the service omitted for a same-service reference). It is set only
	// when IsReference is true, and makes a configured field a worked example of
	// the controller's own conventions.
	ReferenceTarget string `json:"reference_target,omitempty"`
	// IsImmutable reports whether generator.yaml marks the field is_immutable. A
	// supporting signal in favour of a reference, not an exclusion: a KMS key,
	// IAM role, parent ID, or subnet is typically set once.
	IsImmutable bool `json:"is_immutable"`
	// IsPrimaryKey reports whether generator.yaml marks the field
	// is_primary_key. It cuts both ways: the resource's own key is not a
	// reference, but a sub-resource's primary key frequently points at its parent.
	IsPrimaryKey bool `json:"is_primary_key"`
}

// ResourceIndex is the candidate index for one resource of one controller,
// together with what the index cannot show.
type ResourceIndex struct {
	// Controller is the controller alias (for example "eks").
	Controller string `json:"controller"`
	// Resource is the resource Kind.
	Resource string `json:"resource"`
	// Model is the aws-sdk-go-v2 model name the documentation was resolved from.
	Model string `json:"model"`
	// ModelAvailable reports whether the model was fetched and decoded. When
	// false the index is degraded rather than absent: nested fields lose their
	// documentation and patterns, leaving them judgeable by name alone.
	ModelAvailable bool `json:"model_available"`
	// Candidates are the resource's candidate fields, ordered by path.
	Candidates []CandidateRecord `json:"candidates"`
	// Suppressed are identifier-looking fields that generator.yaml removes from
	// the CRD via ignore.field_paths. They can never appear among Candidates
	// however the index is built, and a suppression can hide a reference, so an
	// empty Candidates gap list alongside a non-empty Suppressed is not a clean
	// resource. A suppressed field is also not fixable with a references block:
	// a reference cannot target a field absent from the CRD.
	Suppressed []string `json:"suppressed,omitempty"`
}

// CandidatesOptions selects what to index and where to write it.
type CandidatesOptions struct {
	// Controller is a controller alias, its full "<alias>-controller" form, or
	// All to index every controller under the workspace root.
	Controller string
	// Resource is a resource Kind or All to index every resource the selected
	// controllers declare.
	Resource string
	// OutDir, when non-empty, writes one "<OutDir>/<alias>/<Resource>.jsonl" per
	// resource instead of streaming every record to the writer. This is the form
	// a parallel audit consumes, one file per auditor.
	OutDir string
}

// Indexer builds cross-resource-reference candidate indexes for resources in a
// workspace. It reads each controller's CRDs and generator.yaml locally and
// fetches the service's Smithy API model over HTTP; unlike Scanner it needs no
// Bedrock client and therefore no AWS credentials.
type Indexer struct {
	fetcher ModelFetcher
	out     io.Writer
	errOut  io.Writer
}

// NewIndexer returns an Indexer that writes records to out and progress notes to
// errOut. The GitHub token is optional and only raises the rate limit when
// fetching models from raw.githubusercontent.com.
func NewIndexer(githubToken string, out, errOut io.Writer) *Indexer {
	return &Indexer{
		fetcher: newHTTPModelFetcher(githubToken),
		out:     out,
		errOut:  errOut,
	}
}

// NewIndexerWithFetcher returns an Indexer backed by the given ModelFetcher, so
// tests can supply models without network access.
func NewIndexerWithFetcher(f ModelFetcher, out, errOut io.Writer) *Indexer {
	return &Indexer{fetcher: f, out: out, errOut: errOut}
}

// Candidates builds the candidate index for every selected (controller,
// resource) pair, writing the records and reporting one Result per resource.
//
// A resource declared in generator.yaml with no generated CRD yields a skipped
// Result rather than a failure: the resource is legitimately not indexable, and
// the caller needs to know it was not assessed rather than have the batch abort.
// A model that cannot be fetched degrades every resource of that controller to a
// model-free index and is reported as a note, again without failing.
func (ix *Indexer) Candidates(ctx context.Context, a app.App, opts CandidatesOptions) (workspace.Summary, error) {
	controller := opts.Controller
	if controller == "" {
		controller = All
	}
	resource := opts.Resource
	if resource == "" {
		resource = All
	}

	controllers, err := resolveControllers(a.Config.WorkspaceRoot, controller)
	if err != nil {
		return workspace.Summary{}, err
	}
	if len(controllers) == 0 {
		return workspace.Summary{}, fmt.Errorf("no controllers found under %s", a.Config.WorkspaceRoot)
	}

	var results []workspace.Result
	for _, c := range controllers {
		resources, resErr := resolveResources(c, resource)
		if resErr != nil {
			results = append(results, workspace.Result{
				Repo:    c.Alias,
				Outcome: workspace.OutcomeFailed,
				Reason:  resErr.Error(),
				Err:     resErr,
			})
			ix.notef("%-40s FAIL  %v", c.Alias, resErr)
			continue
		}
		if len(resources) == 0 {
			continue
		}
		results = append(results, ix.indexController(ctx, a, c, resources, opts)...)
	}

	// Only failures are returned in the Summary. The command renders its own
	// per-resource progress on stderr and its records on stdout, so a Summary the
	// entrypoint would print for a successful run corrupts the record stream —
	// but a failure still has to reach the process exit code.
	var summary workspace.Summary
	for _, r := range results {
		if r.Outcome == workspace.OutcomeFailed {
			summary.Results = append(summary.Results, r)
		}
	}
	return summary, nil
}

// indexController indexes the given resources of one controller. The model is
// fetched and decoded once and reused across the controller's resources, which
// matters because decoding is a multi-megabyte JSON unmarshal for the larger
// services and the member-name fallback walks every shape.
func (ix *Indexer) indexController(
	ctx context.Context,
	a app.App,
	c controllerRef,
	resources []string,
	opts CandidatesOptions,
) []workspace.Result {
	modelName := resolveModelName(c.Path, c.Alias)

	var docs modelDocs
	modelAvailable := false
	if a.DryRun {
		ix.notef("%s: would index %d resource(s) from model %s", c.Alias, len(resources), modelName)
		return nil
	}
	raw, err := ix.fetcher.FetchModel(ctx, modelName)
	if err != nil {
		ix.notef("note: %s: model %s unavailable (%v); nested fields will have no description or pattern",
			c.Alias, modelName, err)
	} else if docs, err = newModelDocs(raw); err != nil {
		ix.notef("note: %s: model %s unusable (%v); nested fields will have no description or pattern",
			c.Alias, modelName, err)
	} else {
		modelAvailable = true
	}

	suppressed, err := suppressedIdentifierFields(c.Path)
	if err != nil {
		ix.notef("note: %s: could not read ignore.field_paths (%v); suppressed references cannot be reported", c.Alias, err)
	}

	var results []workspace.Result
	for _, res := range resources {
		label := c.Alias + "/" + res
		idx, err := ix.buildResourceIndex(c, res, modelName, modelAvailable, docs, suppressed)
		if err != nil {
			// A resource declared in generator.yaml without a generated CRD is not
			// indexable. That is a fact about the repo, not a failure of this run,
			// and the caller must be able to tell it apart from a clean index.
			results = append(results, workspace.Result{
				Repo:    label,
				Outcome: workspace.OutcomeSkipped,
				Reason:  err.Error(),
			})
			ix.notef("%-40s SKIP  %v", label, err)
			continue
		}
		if err := ix.emit(idx, opts); err != nil {
			results = append(results, workspace.Result{
				Repo:    label,
				Outcome: workspace.OutcomeFailed,
				Reason:  err.Error(),
				Err:     err,
			})
			continue
		}
		results = append(results, workspace.Result{Repo: label, Outcome: workspace.OutcomeCreated})
		ix.reportResource(idx, opts)
	}

	if len(suppressed) > 0 {
		ix.notef("note: %s: %d identifier-looking field(s) suppressed by ignore.field_paths cannot appear in any "+
			"index. A suppression can hide a reference — check these by hand:", c.Alias, len(suppressed))
		for _, p := range suppressed {
			ix.notef("  %s", p)
		}
	}
	return results
}

// buildResourceIndex assembles one resource's index from the CRD, the
// generator.yaml markings, and the model documentation.
func (ix *Indexer) buildResourceIndex(
	c controllerRef,
	resource, modelName string,
	modelAvailable bool,
	docs modelDocs,
	suppressed []string,
) (ResourceIndex, error) {
	var index docIndex
	if modelAvailable {
		// A resource whose spec sources cannot be read still yields a usable
		// index; it simply loses the model documentation.
		if built, err := docs.indexFor(c.Path, resource); err == nil {
			index = built
		}
	}
	records, err := candidateRecords(c.Path, resource, index)
	if err != nil {
		return ResourceIndex{}, err
	}
	return ResourceIndex{
		Controller:     c.Alias,
		Resource:       resource,
		Model:          modelName,
		ModelAvailable: modelAvailable,
		Candidates:     records,
		Suppressed:     suppressed,
	}, nil
}

// candidateRecords builds the candidate records for one resource: every
// string-valued spec field of its CRD, marked from generator.yaml and enriched
// from the model documentation. A zero docIndex is valid and leaves model-sourced
// descriptions and patterns empty.
func candidateRecords(repoPath, resource string, docs docIndex) ([]CandidateRecord, error) {
	spec, records, err := walkedSpecFieldRecords(repoPath, resource)
	if err != nil {
		return nil, err
	}
	records = filterNonStringFields(records, stringValuedPaths(spec))

	markings, err := loadFieldConfig(repoPath, resource)
	if err != nil {
		return nil, err
	}
	targets, err := loadReferenceTargets(repoPath, resource)
	if err != nil {
		return nil, err
	}

	out := make([]CandidateRecord, 0, len(records))
	for _, r := range records {
		norm := strings.ToLower(r.Path)
		rec := CandidateRecord{
			Resource:     resource,
			Path:         r.Path,
			Type:         r.Type,
			Description:  r.Description,
			IsReference:  markings.ref[norm] || underReferencePrefix(norm, markings.ref),
			IsImmutable:  markings.immutable[norm],
			IsPrimaryKey: markings.primaryKey[norm],
		}
		if rec.Description != "" {
			rec.DescriptionSource = descriptionSourceCRD
		}
		if rec.IsReference {
			rec.ReferenceTarget = targets[norm]
		}
		if doc, origin, ok := docs.lookupOrigin(r.Path); ok {
			rec.Pattern = doc.Pattern
			if rec.Description == "" && doc.Description != "" {
				rec.Description = doc.Description
				rec.DescriptionSource = descriptionSourceModel
			}
			if rec.DescriptionSource == descriptionSourceModel || rec.Pattern != "" {
				rec.ModelJoin = origin
			}
		}
		out = append(out, rec)
	}
	return out, nil
}

// emit writes one resource's records, either to a per-resource file under
// OutDir or as a stream on the Indexer's writer.
func (ix *Indexer) emit(idx ResourceIndex, opts CandidatesOptions) error {
	body, err := marshalCandidateLines(idx.Candidates)
	if err != nil {
		return err
	}
	if opts.OutDir == "" {
		_, err := io.WriteString(ix.out, body)
		return err
	}
	dir := filepath.Join(opts.OutDir, idx.Controller)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, idx.Resource+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// marshalCandidateLines renders the records as JSON Lines: one self-contained
// JSON object per line, so a consumer can grep whole records and read the file
// incrementally.
func marshalCandidateLines(records []CandidateRecord) (string, error) {
	var b strings.Builder
	for _, r := range records {
		line, err := json.Marshal(r)
		if err != nil {
			return "", fmt.Errorf("marshalling candidate %s: %w", r.Path, err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// reportResource prints the per-resource progress line, which doubles as the
// record of what the run covered. Configured references are counted separately
// from unconfigured candidates because the second number is the audit's workload.
func (ix *Indexer) reportResource(idx ResourceIndex, opts CandidatesOptions) {
	configured := 0
	for _, r := range idx.Candidates {
		if r.IsReference {
			configured++
		}
	}
	dest := "-"
	if opts.OutDir != "" {
		dest = filepath.Join(opts.OutDir, idx.Controller, idx.Resource+".jsonl")
	}
	model := idx.Model
	if !idx.ModelAvailable {
		model += " (unavailable)"
	}
	ix.notef("%-40s candidates=%-5d configured=%-4d model=%-28s -> %s",
		idx.Controller+"/"+idx.Resource, len(idx.Candidates), configured, model, dest)
}

// notef writes a progress or diagnostic line to the Indexer's note writer. Notes
// go to stderr so they never corrupt the records on stdout.
func (ix *Indexer) notef(format string, args ...any) {
	if ix.errOut == nil {
		return
	}
	fmt.Fprintf(ix.errOut, format+"\n", args...)
}

// generatorReferenceTargets decodes the configured target of every field that
// carries a references block.
type generatorReferenceTargets struct {
	Resources map[string]struct {
		Fields map[string]struct {
			References *struct {
				Resource    string `yaml:"resource"`
				ServiceName string `yaml:"service_name"`
				Path        string `yaml:"path"`
			} `yaml:"references"`
		} `yaml:"fields"`
	} `yaml:"resources"`
}

// loadReferenceTargets returns the configured reference target for each of the
// resource's reference fields, keyed by lowercased field path so it correlates
// case-insensitively with the CRD's camelCase paths. A configured field is a
// worked example of the controller's own conventions — which path form it uses,
// whether it sets service_name — so surfacing the target saves a reader a trip
// to generator.yaml.
func loadReferenceTargets(repoPath, resource string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(repoPath, generatorFileName))
	if err != nil {
		return nil, fmt.Errorf("reading generator.yaml: %w", err)
	}
	var g generatorReferenceTargets
	if err := yaml.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("parsing generator.yaml: %w", err)
	}
	out := map[string]string{}
	for path, fc := range g.Resources[resource].Fields {
		if fc.References == nil {
			continue
		}
		target := fc.References.Resource
		if fc.References.ServiceName != "" {
			target = fc.References.ServiceName + " " + target
		}
		if fc.References.Path != "" {
			target += " -> " + fc.References.Path
		}
		out[strings.ToLower(path)] = target
	}
	return out, nil
}

// generatorIgnore decodes the ignore.field_paths list, the fields generator.yaml
// removes from the CRD entirely.
type generatorIgnore struct {
	Ignore struct {
		FieldPaths []string `yaml:"field_paths"`
	} `yaml:"ignore"`
}

// identifierSuffixes are the field-name endings that mark a suppressed field as
// worth a human look: a suppressed field with one of these names may be a
// cross-resource reference that no index can see.
var identifierSuffixes = []string{"arn", "arns", "id", "ids", "identifier", "name", "names"}

// suppressedIdentifierFields returns the ignore.field_paths entries whose final
// segment looks like a resource identifier, sorted.
//
// This is the one blind spot no candidate index can close. A suppressed field
// never reaches the CRD, so it cannot appear among the candidates however the
// index is built — and suppressions do hide real references (mq removes
// CreateBrokerInput.DataReplicationPrimaryBrokerArn, a Broker-to-Broker
// reference). Reporting them is what stops an empty gap list from being read as a
// clean resource. Note that a suppressed field is not fixable with a references
// block at all: un-ignoring it is a separate, larger change, because a reference
// cannot target a field absent from the CRD.
func suppressedIdentifierFields(repoPath string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(repoPath, generatorFileName))
	if err != nil {
		return nil, fmt.Errorf("reading generator.yaml: %w", err)
	}
	var g generatorIgnore
	if err := yaml.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("parsing generator.yaml: %w", err)
	}
	var out []string
	for _, p := range g.Ignore.FieldPaths {
		if looksLikeIdentifier(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// looksLikeIdentifier reports whether a dotted field path's final segment ends in
// an identifier suffix, compared case-insensitively so it catches both the
// model's "KmsKeyId" and ACK's "KMSKeyID".
func looksLikeIdentifier(path string) bool {
	segment := path
	if i := strings.LastIndex(segment, "."); i >= 0 {
		segment = segment[i+1:]
	}
	lower := strings.ToLower(segment)
	for _, suffix := range identifierSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}
