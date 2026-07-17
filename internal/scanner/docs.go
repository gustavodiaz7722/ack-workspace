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

// DocsFetcher retrieves Terraform AWS provider documentation. It is an interface
// so the production HTTP implementation can be swapped for an in-memory fake in
// tests, keeping tool execution free of network access.
//
// It is intentionally low-level: it lists the available resource doc slugs and
// returns the full markdown for one slug. The line-oriented operations the agent
// needs (keyword search with line numbers, and reading a line range) are built
// on top of FetchDoc by the tools, so the model can investigate a document
// incrementally.
type DocsFetcher interface {
	// ListResources returns all available Terraform AWS provider resource doc
	// slugs (for example "acm_certificate"), sorted. Filtering is the caller's
	// concern (done with grep), not the fetcher's.
	ListResources(ctx context.Context) ([]string, error)
	// FetchDoc returns the full raw markdown for a resource doc slug. The result
	// is suitable for line-based search and range extraction.
	FetchDoc(ctx context.Context, slug string) (string, error)
}

// terraformDocsRawBaseURL is the raw markdown location of the Terraform AWS
// provider resource documentation. The slug and extension are appended.
const terraformDocsRawBaseURL = "https://raw.githubusercontent.com/hashicorp/terraform-provider-aws/main/website/docs/r/"

// terraformDocsTreeURL is the GitHub Git Trees API view of the provider's
// resource documentation directory, used to enumerate available doc slugs.
//
// The Trees API is used instead of the Contents API deliberately: the Contents
// API caps a directory listing at 1,000 entries and silently truncates, and
// this directory holds well over 1,000 files, so late-alphabet resources (for
// example sns_topic_subscription) would be missing. A single non-recursive Trees
// call returns every entry (up to 100,000) with an explicit truncated flag.
const terraformDocsTreeURL = "https://api.github.com/repos/hashicorp/terraform-provider-aws/git/trees/main:website/docs/r"

// docExtensions are the file suffixes provider docs use, longest first so the
// full compound extension is stripped before a shorter one.
var docExtensions = []string{".html.markdown", ".html.md", ".markdown", ".md"}

// maxDocsBytes caps how much of a single doc is read from the network, a
// defensive bound against a surprise large page.
const maxDocsBytes = 1024 * 1024

// httpDocsFetcher is the production DocsFetcher: it lists docs via the GitHub
// contents API and downloads doc markdown over HTTP, caching each fetched
// document so repeated line queries within a conversation do not refetch.
type httpDocsFetcher struct {
	client *http.Client
	// token, when non-empty, authenticates GitHub requests so the listing API is
	// billed against the (much higher) authenticated rate limit instead of the
	// 60/hour unauthenticated limit.
	token string
	// rawBaseURL and treeURL are the documentation endpoints; they are fields
	// (defaulting to the package constants) so tests can point the fetcher at an
	// httptest server without network access.
	rawBaseURL string
	treeURL    string

	mu    sync.Mutex
	cache map[string]string
}

// newHTTPDocsFetcher returns a DocsFetcher backed by an HTTP client with a
// conservative timeout and an empty document cache. A non-empty token
// authenticates GitHub requests to avoid throttling.
func newHTTPDocsFetcher(token string) DocsFetcher {
	return &httpDocsFetcher{
		client:     &http.Client{Timeout: 15 * time.Second},
		token:      token,
		rawBaseURL: terraformDocsRawBaseURL,
		treeURL:    terraformDocsTreeURL,
		cache:      map[string]string{},
	}
}

// authorize adds GitHub authentication headers to req when a token is
// configured. It is a no-op otherwise, so anonymous use still works (subject to
// the lower unauthenticated rate limit).
func (f *httpDocsFetcher) authorize(req *http.Request) {
	if f.token == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

// treeResponse is the subset of a Git Trees API response we read. Truncated is
// set by GitHub when the directory exceeds the API's per-response entry limit;
// for this directory (well under the limit) it is always false, but it is
// surfaced so an incomplete listing can be detected rather than silently used.
type treeResponse struct {
	Tree      []treeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

// treeEntry is one entry of a Git tree. For a non-recursive tree of a directory,
// Path is the bare entry name (for example "sns_topic_subscription.html.markdown").
type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"` // "blob" for files, "tree" for subdirectories
}

// ListResources enumerates all of the provider's resource doc slugs, sorted.
func (f *httpDocsFetcher) ListResources(ctx context.Context) ([]string, error) {
	resp, err := getWithRetry(ctx, f.client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.treeURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		f.authorize(req)
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing terraform docs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing terraform docs: unexpected status %s", resp.Status)
	}

	var tree treeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16*maxDocsBytes)).Decode(&tree); err != nil {
		return nil, fmt.Errorf("decoding terraform docs listing: %w", err)
	}
	if tree.Truncated {
		return nil, fmt.Errorf("terraform docs listing was truncated by the GitHub API; results would be incomplete")
	}

	var slugs []string
	for _, e := range tree.Tree {
		if e.Type != "blob" {
			continue
		}
		if slug := stripDocExtension(e.Path); slug != "" {
			slugs = append(slugs, slug)
		}
	}
	sort.Strings(slugs)
	return slugs, nil
}

// FetchDoc returns the full markdown for slug, serving a cached copy when one is
// present. A 404 (unknown slug) is reported as an error so the model can retry.
func (f *httpDocsFetcher) FetchDoc(ctx context.Context, slug string) (string, error) {
	if cached, ok := f.cached(slug); ok {
		return cached, nil
	}

	url := f.rawBaseURL + slug + ".html.markdown"
	resp, err := getWithRetry(ctx, f.client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		f.authorize(req)
		return req, nil
	})
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching %s: unexpected status %s", url, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocsBytes))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", url, err)
	}
	doc := string(body)
	f.store(slug, doc)
	return doc, nil
}

// cached returns a previously fetched document for slug, if present.
func (f *httpDocsFetcher) cached(slug string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	doc, ok := f.cache[slug]
	return doc, ok
}

// store caches a fetched document.
func (f *httpDocsFetcher) store(slug, doc string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cache[slug] = doc
}

// stripDocExtension removes a known documentation file extension from name,
// returning the bare slug. A name without a known extension yields "".
func stripDocExtension(name string) string {
	for _, ext := range docExtensions {
		if strings.HasSuffix(name, ext) {
			return strings.TrimSuffix(name, ext)
		}
	}
	return ""
}

// countLines returns the number of lines in markdown, reported alongside grep
// results so the model knows the document's size.
func countLines(markdown string) int {
	if markdown == "" {
		return 0
	}
	return strings.Count(markdown, "\n") + 1
}
