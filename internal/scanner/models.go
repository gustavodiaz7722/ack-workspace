package scanner

import (
	"context"
	"fmt"
	"io"
	"net/http"
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
// The model name and ".json" suffix are appended. Models are read from the
// SDK's main branch: the exact SDK version a controller was generated against
// is not needed to reason about which fields are references.
const modelsRawBaseURL = "https://raw.githubusercontent.com/aws/aws-sdk-go-v2/main/codegen/sdk-codegen/aws-models/"

// maxModelBytes caps how much of a single model is read from the network.
// Smithy models range from a few KB to several MB (EC2 is the largest at well
// under 32MB), so this is a generous defensive bound rather than an expected
// limit.
const maxModelBytes = 32 * 1024 * 1024

// httpModelFetcher is the production ModelFetcher: it downloads model JSON over
// HTTP, caching each fetched model so repeated grep queries within a
// conversation (and across a controller's resources) do not refetch.
type httpModelFetcher struct {
	client *http.Client
	// token, when non-empty, authenticates the raw GitHub request. Raw content is
	// public, so this is only about staying clear of anonymous rate limits.
	token string
	// rawBaseURL is the model endpoint; it is a field (defaulting to the package
	// constant) so tests can point the fetcher at an httptest server.
	rawBaseURL string

	mu    sync.Mutex
	cache map[string]string
}

// newHTTPModelFetcher returns a ModelFetcher backed by an HTTP client with a
// conservative timeout and an empty model cache. A non-empty token
// authenticates the request to avoid throttling.
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
	resp, err := getWithRetry(ctx, f.client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if f.token != "" {
			req.Header.Set("Authorization", "Bearer "+f.token)
		}
		return req, nil
	})
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

// shortShapeName returns the local part of a Smithy shape id, dropping the
// "com.amazonaws.<service>#" namespace prefix (for example
// "com.amazonaws.acm#CertificateDetail" -> "CertificateDetail").
func shortShapeName(name string) string {
	if i := strings.LastIndex(name, "#"); i >= 0 {
		return name[i+1:]
	}
	return name
}
