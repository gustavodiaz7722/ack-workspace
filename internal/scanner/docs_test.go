package scanner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestFetcher wires an httpDocsFetcher at the given test server so the real
// endpoints are never contacted.
func newTestFetcher(srv *httptest.Server, token string) *httpDocsFetcher {
	return &httpDocsFetcher{
		client:     srv.Client(),
		token:      token,
		rawBaseURL: srv.URL + "/raw/",
		treeURL:    srv.URL + "/tree",
		cache:      map[string]string{},
	}
}

func TestHTTPDocsFetcherSendsToken(t *testing.T) {
	var listAuth, rawAuth string
	var rawCalls int

	mux := http.NewServeMux()
	mux.HandleFunc("/tree", func(w http.ResponseWriter, r *http.Request) {
		listAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"truncated":false,"tree":[
			{"path":"acm_certificate.html.markdown","type":"blob"},
			{"path":"s3_bucket.html.markdown","type":"blob"},
			{"path":"guides","type":"tree"}
		]}`))
	})
	mux.HandleFunc("/raw/acm_certificate.html.markdown", func(w http.ResponseWriter, r *http.Request) {
		rawAuth = r.Header.Get("Authorization")
		rawCalls++
		_, _ = w.Write([]byte("line one\nline two\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := newTestFetcher(srv, "secret-token")
	ctx := context.Background()

	// Listing authenticates and returns every file (blobs only, "guides" tree
	// excluded); filtering is the caller's job, not the fetcher's.
	slugs, err := f.ListResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(slugs) != 2 || slugs[0] != "acm_certificate" || slugs[1] != "s3_bucket" {
		t.Errorf("slugs = %v, want [acm_certificate s3_bucket]", slugs)
	}
	if listAuth != "Bearer secret-token" {
		t.Errorf("listing Authorization = %q, want Bearer secret-token", listAuth)
	}

	// Fetch authenticates too, and the result is cached (only one server hit).
	for i := 0; i < 2; i++ {
		doc, err := f.FetchDoc(ctx, "acm_certificate")
		if err != nil {
			t.Fatal(err)
		}
		if doc == "" {
			t.Fatal("expected document content")
		}
	}
	if rawCalls != 1 {
		t.Errorf("raw endpoint called %d times, want 1 (cache miss then hit)", rawCalls)
	}
	if rawAuth != "Bearer secret-token" {
		t.Errorf("fetch Authorization = %q, want Bearer secret-token", rawAuth)
	}
}

func TestHTTPDocsFetcherAnonymousNoAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"truncated":false,"tree":[]}`))
	}))
	defer srv.Close()

	f := newTestFetcher(srv, "")
	if _, err := f.ListResources(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Errorf("anonymous request sent Authorization = %q, want none", gotAuth)
	}
}

func TestHTTPDocsFetcherRejectsTruncatedListing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"truncated":true,"tree":[{"path":"acm_certificate.html.markdown","type":"blob"}]}`))
	}))
	defer srv.Close()

	f := newTestFetcher(srv, "")
	if _, err := f.ListResources(context.Background()); err == nil {
		t.Error("a truncated listing should be rejected as incomplete, got nil error")
	}
}
