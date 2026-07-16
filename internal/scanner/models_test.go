package scanner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testSmithyModel is a minimal Smithy model with a reference member (RoleArn,
// whose target shape carries the arnReference trait), a non-reference member,
// and an unrelated shape used to verify filtering.
const testSmithyModel = `{
  "smithy": "2.0",
  "shapes": {
    "com.amazonaws.acm#CertificateDetail": {
      "type": "structure",
      "members": {
        "RoleArn": {
          "target": "com.amazonaws.acm#RoleArnType",
          "traits": { "smithy.api#documentation": "<p>The ARN of the IAM role.</p>" }
        },
        "DomainName": {
          "target": "com.amazonaws.acm#DomainName",
          "traits": { "smithy.api#documentation": "<p>The domain name.</p>" }
        }
      }
    },
    "com.amazonaws.acm#RoleArnType": {
      "type": "string",
      "traits": { "aws.api#arnReference": {} }
    },
    "com.amazonaws.acm#DomainName": { "type": "string" },
    "com.amazonaws.acm#UnrelatedShape": {
      "type": "structure",
      "members": { "Foo": { "target": "com.amazonaws.acm#DomainName" } }
    }
  }
}`

func newTestModelFetcher(srv *httptest.Server, token string) *httpModelFetcher {
	return &httpModelFetcher{
		client:     srv.Client(),
		token:      token,
		rawBaseURL: srv.URL + "/",
		cache:      map[string]string{},
	}
}

func TestHTTPModelFetcherCachesAndSendsToken(t *testing.T) {
	var gotAuth string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		calls++
		if !strings.HasSuffix(r.URL.Path, "/acm.json") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(testSmithyModel))
	}))
	defer srv.Close()

	f := newTestModelFetcher(srv, "secret")
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		m, err := f.FetchModel(ctx, "acm")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(m, "RoleArn") {
			t.Fatalf("model content unexpected:\n%s", m)
		}
	}
	if calls != 1 {
		t.Errorf("model endpoint called %d times, want 1 (miss then cache hit)", calls)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want Bearer secret", gotAuth)
	}
}

func TestHTTPModelFetcherUnknownModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()
	f := newTestModelFetcher(srv, "")
	if _, err := f.FetchModel(context.Background(), "nope"); err == nil {
		t.Error("expected an error for an unknown model")
	}
}

func TestFilterModelContent(t *testing.T) {
	out := filterModelContent(testSmithyModel, "Certificate")

	// The resource's structure is kept, and so is the target shape carrying the
	// arnReference trait (pulled in one level out).
	if !strings.Contains(out, "CertificateDetail") {
		t.Errorf("filtered model dropped the resource structure:\n%s", out)
	}
	if !strings.Contains(out, "arnReference") || !strings.Contains(out, "RoleArnType") {
		t.Errorf("filtered model dropped the arnReference target shape:\n%s", out)
	}
	// An unrelated structure whose name does not contain the resource kind is
	// dropped.
	if strings.Contains(out, "UnrelatedShape") {
		t.Errorf("filtered model kept an unrelated shape:\n%s", out)
	}
}

func TestFilterModelContentNoMatchFallsBack(t *testing.T) {
	out := filterModelContent(testSmithyModel, "Nonexistent")
	if out != testSmithyModel {
		t.Error("with no matching shapes, the full model should be returned unchanged")
	}
}

func TestFilterModelContentUnparseableFallsBack(t *testing.T) {
	if out := filterModelContent("not json", "Certificate"); out != "not json" {
		t.Error("unparseable content should be returned unchanged")
	}
}
