package scanner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testSmithyModel is a minimal Smithy model shaped like the real thing for the
// purposes of the doc-index walk: a Create<Kind>Request root, a member whose
// target shape carries an ARN pattern, a list of structures (to exercise the
// list unwrap), a union, a renamed member, a second operation that a custom
// field sources from, and an ambiguous member name (Description, declared twice
// with different text) that the member-name fallback must refuse to use.
const testSmithyModel = `{
  "smithy": "2.0",
  "shapes": {
    "com.amazonaws.acm#CreateCertificateRequest": {
      "type": "structure",
      "members": {
        "CertificateName": {
          "target": "com.amazonaws.acm#DomainName",
          "traits": { "smithy.api#documentation": "<p>The certificate name.</p>" }
        },
        "DomainName": {
          "target": "com.amazonaws.acm#DomainName",
          "traits": { "smithy.api#documentation": "<p>The domain name.</p>" }
        },
        "RoleArn": {
          "target": "com.amazonaws.acm#RoleArnType",
          "traits": { "smithy.api#documentation": "<p>The ARN of the IAM role.</p>" }
        },
        "Description": {
          "target": "com.amazonaws.acm#DomainName",
          "traits": { "smithy.api#documentation": "<p>A certificate description.</p>" }
        },
        "Tags": { "target": "com.amazonaws.acm#TagList" },
        "Options": { "target": "com.amazonaws.acm#OptionsUnion" }
      }
    },
    "com.amazonaws.acm#AddPermissionRequest": {
      "type": "structure",
      "members": {
        "SourceArn": {
          "target": "com.amazonaws.acm#RoleArnType",
          "traits": { "smithy.api#documentation": "<p>The ARN of the calling service.</p>" }
        }
      }
    },
    "com.amazonaws.acm#TagList": {
      "type": "list",
      "member": { "target": "com.amazonaws.acm#Tag" }
    },
    "com.amazonaws.acm#Tag": {
      "type": "structure",
      "members": {
        "Key": {
          "target": "com.amazonaws.acm#DomainName",
          "traits": { "smithy.api#documentation": "<p>The tag key.</p>" }
        },
        "Value": {
          "target": "com.amazonaws.acm#DomainName",
          "traits": { "smithy.api#documentation": "<p>The tag value.</p>" }
        }
      }
    },
    "com.amazonaws.acm#OptionsUnion": {
      "type": "union",
      "members": {
        "KmsKeyId": {
          "target": "com.amazonaws.acm#DomainName",
          "traits": { "smithy.api#documentation": "<p>The ID of the KMS key.</p>" }
        }
      }
    },
    "com.amazonaws.acm#Unrelated": {
      "type": "structure",
      "members": {
        "Description": {
          "target": "com.amazonaws.acm#DomainName",
          "traits": { "smithy.api#documentation": "<p>Something else entirely.</p>" }
        }
      }
    },
    "com.amazonaws.acm#RoleArnType": {
      "type": "string",
      "traits": { "smithy.api#pattern": "^arn:aws:iam::\\d{12}:role/.+$" }
    },
    "com.amazonaws.acm#DomainName": { "type": "string" }
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

func TestShortShapeName(t *testing.T) {
	if got := shortShapeName("com.amazonaws.acm#CertificateDetail"); got != "CertificateDetail" {
		t.Errorf("shortShapeName = %q, want CertificateDetail", got)
	}
	if got := shortShapeName("NoNamespace"); got != "NoNamespace" {
		t.Errorf("shortShapeName = %q, want NoNamespace", got)
	}
}
