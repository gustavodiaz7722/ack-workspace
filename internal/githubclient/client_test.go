package githubclient

import (
	"context"
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
