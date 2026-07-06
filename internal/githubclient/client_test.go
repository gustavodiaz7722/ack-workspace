package githubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v66/github"
)

// roundTripFunc adapts a function to an http.RoundTripper so tests can return
// canned responses without a network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newStubAdapter builds an Adapter whose underlying go-github client is backed
// by the supplied RoundTripper, plus any adapter options.
func newStubAdapter(rt http.RoundTripper, opts ...Option) *Adapter {
	httpClient := &http.Client{Transport: rt}
	return newAdapter(github.NewClient(httpClient), opts...)
}

// jsonResp builds a canned JSON *http.Response for the given status and body.
func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func TestAdapterRepoExists(t *testing.T) {
	ref := RepoRef{Owner: "aws-controllers-k8s", Name: "runtime"}

	transportErr := errors.New("dial tcp: connection refused")

	tests := []struct {
		name       string
		rt         roundTripFunc
		wantExists bool
		wantErr    bool
	}{
		{
			name: "200 OK returns exists",
			rt: func(*http.Request) (*http.Response, error) {
				return jsonResp(http.StatusOK, `{"name":"runtime"}`), nil
			},
			wantExists: true,
			wantErr:    false,
		},
		{
			name: "404 returns not found without error",
			rt: func(*http.Request) (*http.Response, error) {
				return jsonResp(http.StatusNotFound, `{"message":"Not Found"}`), nil
			},
			wantExists: false,
			wantErr:    false,
		},
		{
			name: "500 returns an error",
			rt: func(*http.Request) (*http.Response, error) {
				return jsonResp(http.StatusInternalServerError, `{"message":"server error"}`), nil
			},
			wantExists: false,
			wantErr:    true,
		},
		{
			name: "transport error returns an error",
			rt: func(*http.Request) (*http.Response, error) {
				return nil, transportErr
			},
			wantExists: false,
			wantErr:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newStubAdapter(tc.rt)
			got, err := a.RepoExists(context.Background(), ref)
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantExists {
				t.Fatalf("exists = %v, want %v", got, tc.wantExists)
			}
		})
	}
}

func TestAdapterDefaultBranch(t *testing.T) {
	ref := RepoRef{Owner: "aws-controllers-k8s", Name: "runtime"}

	a := newStubAdapter(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, `{"name":"runtime","default_branch":"main"}`), nil
	}))

	got, err := a.DefaultBranch(context.Background(), ref)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "main" {
		t.Fatalf("default branch = %q, want %q", got, "main")
	}
}

func TestAdapterDefaultBranchError(t *testing.T) {
	ref := RepoRef{Owner: "aws-controllers-k8s", Name: "runtime"}

	a := newStubAdapter(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResp(http.StatusInternalServerError, `{"message":"boom"}`), nil
	}))

	if _, err := a.DefaultBranch(context.Background(), ref); err == nil {
		t.Fatalf("expected an error, got nil")
	}
}

func TestAdapterCreateForkSuccess(t *testing.T) {
	upstream := RepoRef{Owner: "aws-controllers-k8s", Name: "runtime"}
	const forkName = "ack-runtime"

	var (
		mu       sync.Mutex
		getCalls int
	)

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/forks"):
			// The fork must be created with default_branch_only so no upstream
			// branches or tags are copied into the fork.
			var body struct {
				Name              string `json:"name"`
				DefaultBranchOnly bool   `json:"default_branch_only"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decoding fork request body: %v", err)
			}
			if !body.DefaultBranchOnly {
				t.Errorf("fork request default_branch_only = false, want true")
			}
			if body.Name != forkName {
				t.Errorf("fork request name = %q, want %q", body.Name, forkName)
			}
			// Fork request accepted; return the new repository's metadata.
			return jsonResp(http.StatusOK, `{"name":"ack-runtime","owner":{"login":"octocat"}}`), nil
		case r.Method == http.MethodGet:
			// First poll: fork not yet queryable; second poll: present.
			mu.Lock()
			getCalls++
			n := getCalls
			mu.Unlock()
			if n == 1 {
				return jsonResp(http.StatusNotFound, `{"message":"Not Found"}`), nil
			}
			return jsonResp(http.StatusOK, `{"name":"ack-runtime"}`), nil
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})

	a := newStubAdapter(rt,
		WithPollInterval(time.Millisecond),
		WithPollTimeout(time.Second),
	)

	fork, err := a.CreateFork(context.Background(), upstream, forkName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := RepoRef{Owner: "octocat", Name: "ack-runtime"}
	if fork != want {
		t.Fatalf("fork = %+v, want %+v", fork, want)
	}
	if getCalls < 2 {
		t.Fatalf("expected polling to query the fork at least twice, got %d", getCalls)
	}
}

func TestAdapterCreateForkTimeout(t *testing.T) {
	upstream := RepoRef{Owner: "aws-controllers-k8s", Name: "runtime"}
	const forkName = "ack-runtime"

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/forks"):
			return jsonResp(http.StatusOK, `{"name":"ack-runtime","owner":{"login":"octocat"}}`), nil
		case r.Method == http.MethodGet:
			// The fork never becomes queryable.
			return jsonResp(http.StatusNotFound, `{"message":"Not Found"}`), nil
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})

	a := newStubAdapter(rt,
		WithPollInterval(time.Millisecond),
		WithPollTimeout(5*time.Millisecond),
	)

	_, err := a.CreateFork(context.Background(), upstream, forkName)
	if err == nil {
		t.Fatalf("expected a timeout error, got nil")
	}
	var timeoutErr *ForkTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error = %v, want *ForkTimeoutError", err)
	}
	if want := (RepoRef{Owner: "octocat", Name: "ack-runtime"}); timeoutErr.Fork != want {
		t.Fatalf("timeout fork = %+v, want %+v", timeoutErr.Fork, want)
	}
}

// jsonRespWithHeader is like jsonResp but lets a test add response headers (for
// example a Link header to drive go-github's pagination).
func jsonRespWithHeader(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     header,
	}
}

func TestAdapterListOrgRepos(t *testing.T) {
	const org = "aws-controllers-k8s"

	var (
		mu    sync.Mutex
		pages int
	)

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		mu.Lock()
		pages++
		page := pages
		mu.Unlock()

		switch page {
		case 1:
			// First page links to a next page; includes one archived repo that
			// must be filtered out.
			h := http.Header{}
			h.Set("Link", `<https://api.github.com/organizations/1/repos?page=2>; rel="next"`)
			return jsonRespWithHeader(http.StatusOK, `[
				{"name":"runtime","archived":false},
				{"name":"s3-controller","archived":false},
				{"name":"old-controller","archived":true}
			]`, h), nil
		case 2:
			return jsonRespWithHeader(http.StatusOK, `[
				{"name":"sns-controller","archived":false}
			]`, nil), nil
		default:
			t.Fatalf("unexpected extra page request (page %d)", page)
			return nil, nil
		}
	})

	a := newStubAdapter(rt)

	got, err := a.ListOrgRepos(context.Background(), org)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both pages must have been read, and the archived repo excluded.
	want := []string{"runtime", "s3-controller", "sns-controller"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if pages != 2 {
		t.Errorf("expected 2 pages to be fetched, got %d", pages)
	}
}

func TestAdapterListOrgReposError(t *testing.T) {
	a := newStubAdapter(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResp(http.StatusInternalServerError, `{"message":"boom"}`), nil
	}))

	if _, err := a.ListOrgRepos(context.Background(), "aws-controllers-k8s"); err == nil {
		t.Fatalf("expected an error, got nil")
	}
}

func TestAdapterDeleteRepo(t *testing.T) {
	ref := RepoRef{Owner: "octocat", Name: "ack-s3-controller"}

	t.Run("success", func(t *testing.T) {
		var sawDelete bool
		a := newStubAdapter(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodDelete {
				sawDelete = true
				return jsonResp(http.StatusNoContent, ``), nil
			}
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			return nil, nil
		}))
		if err := a.DeleteRepo(context.Background(), ref); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !sawDelete {
			t.Error("expected a DELETE request to be issued")
		}
	})

	t.Run("error", func(t *testing.T) {
		a := newStubAdapter(roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResp(http.StatusForbidden, `{"message":"Must have admin rights"}`), nil
		}))
		if err := a.DeleteRepo(context.Background(), ref); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})
}
