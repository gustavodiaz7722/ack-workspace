package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ModelFetcher retrieves an AWS Smithy JSON API model for a service. It is an
// interface so the production HTTP implementation can be swapped for an
// in-memory fake in tests, keeping tool execution free of network access.
//
// A model is identified by its aws-sdk-go-v2 model name (for example "acm",
// "acm-pca", or "cognito-identity-provider"), which is derived from the
// controller (see resolveModelName). The returned string is the full raw model
// JSON, suitable for line-based grep.
type ModelFetcher interface {
	// FetchModel returns the full raw Smithy model JSON for a model name.
	FetchModel(ctx context.Context, modelName string) (string, error)
}

// modelsRawBaseURL is the raw location of the aws-sdk-go-v2 Smithy API models.
// The model name and ".json" suffix are appended. The scanner reads the models
// from the SDK's main branch, mirroring how the Terraform docs are read from
// their provider's main branch; the exact SDK version a controller was
// generated against is not needed to reason about which fields are references.
const modelsRawBaseURL = "https://raw.githubusercontent.com/aws/aws-sdk-go-v2/main/codegen/sdk-codegen/aws-models/"

// maxModelBytes caps how much of a single model is read from the network. Smithy
// models range from a few KB to several MB (EC2 is the largest at well under
// 32MB), so this is a generous defensive bound rather than an expected limit.
const maxModelBytes = 32 * 1024 * 1024

// httpModelFetcher is the production ModelFetcher: it downloads model JSON over
// HTTP, caching each fetched model so repeated grep queries within a
// conversation (and across a controller's resources) do not refetch.
type httpModelFetcher struct {
	client *http.Client
	// token, when non-empty, authenticates the raw GitHub request. Raw content
	// is public, so this is only about staying clear of anonymous rate limits.
	token string
	// rawBaseURL is the model endpoint; it is a field (defaulting to the package
	// constant) so tests can point the fetcher at an httptest server.
	rawBaseURL string

	mu    sync.Mutex
	cache map[string]string
}

// newHTTPModelFetcher returns a ModelFetcher backed by an HTTP client with a
// conservative timeout and an empty model cache. A non-empty token authenticates
// the request to avoid throttling.
func newHTTPModelFetcher(token string) ModelFetcher {
	return &httpModelFetcher{
		client:     &http.Client{Timeout: 30 * time.Second},
		token:      token,
		rawBaseURL: modelsRawBaseURL,
		cache:      map[string]string{},
	}
}

// FetchModel returns the full model JSON for modelName, serving a cached copy
// when present. A non-200 (for example an unknown model name) is reported as an
// error so the model can retry with a different name.
func (f *httpModelFetcher) FetchModel(ctx context.Context, modelName string) (string, error) {
	if cached, ok := f.cached(modelName); ok {
		return cached, nil
	}

	url := f.rawBaseURL + modelName + ".json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching model %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching model %s: unexpected status %s", url, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelBytes))
	if err != nil {
		return "", fmt.Errorf("reading model %s: %w", url, err)
	}
	model := string(body)
	f.store(modelName, model)
	return model, nil
}

func (f *httpModelFetcher) cached(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.cache[name]
	return m, ok
}

func (f *httpModelFetcher) store(name, model string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cache[name] = model
}

// filterModelContent reduces a full Smithy model to the shapes relevant to one
// resource, keeping the raw JSON structure (so the aws.api#arnReference traits
// and member documentation the reference issue depends on stay intact) while
// cutting the volume the agent must grep.
//
// Selection is by shape name: any shape whose short name (the part after "#")
// contains the resource kind case-insensitively is kept, along with the shapes
// its members target (one level out), so a member's target shape — which is
// where an arnReference trait usually lives — is visible alongside it. When the
// model cannot be parsed or nothing matches, the full model is returned so the
// agent still has something to search.
func filterModelContent(content, resource string) string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &top); err != nil {
		return content
	}
	shapesRaw, ok := top["shapes"]
	if !ok {
		return content
	}
	var shapes map[string]json.RawMessage
	if err := json.Unmarshal(shapesRaw, &shapes); err != nil {
		return content
	}

	keyword := strings.ToLower(resource)
	relevant := map[string]json.RawMessage{}
	for name, data := range shapes {
		if strings.Contains(strings.ToLower(shortShapeName(name)), keyword) {
			relevant[name] = data
		}
	}
	if len(relevant) == 0 {
		return content
	}

	// Pull in the shapes that the selected structures' members target, so a
	// member and the shape carrying its arnReference trait are searchable
	// together.
	for _, data := range collect(relevant) {
		for _, target := range memberTargets(data) {
			if td, ok := shapes[target]; ok {
				if _, seen := relevant[target]; !seen {
					relevant[target] = td
				}
			}
		}
	}

	filtered, err := json.MarshalIndent(map[string]any{"shapes": relevant}, "", "    ")
	if err != nil {
		return content
	}
	return string(filtered)
}

// collect returns the values of a shape map as a slice so iteration is not
// affected by concurrent insertion into the same map.
func collect(m map[string]json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// memberTargets returns the target shape names of a structure shape's members,
// sorted for determinism. Non-structure shapes yield no targets.
func memberTargets(shapeData json.RawMessage) []string {
	var shape struct {
		Type    string `json:"type"`
		Members map[string]struct {
			Target string `json:"target"`
		} `json:"members"`
	}
	if err := json.Unmarshal(shapeData, &shape); err != nil {
		return nil
	}
	var targets []string
	for _, m := range shape.Members {
		if m.Target != "" {
			targets = append(targets, m.Target)
		}
	}
	sort.Strings(targets)
	return targets
}

// shortShapeName returns the local part of a Smithy shape id, dropping the
// "com.amazonaws.<service>#" namespace prefix (for example
// "com.amazonaws.acm#CertificateDetail" -> "CertificateDetail").
func shortShapeName(name string) string {
	if i := strings.LastIndex(name, "#"); i >= 0 {
		return name[i+1:]
	}
	return name
}
