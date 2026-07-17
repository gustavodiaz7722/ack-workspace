package scanner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// withFastBackoff shrinks the retry backoff for the duration of a test so retry
// paths do not add real delay, restoring it afterward.
func withFastBackoff(t *testing.T) {
	t.Helper()
	prev := httpRetryBackoff
	httpRetryBackoff = time.Millisecond
	t.Cleanup(func() { httpRetryBackoff = prev })
}

func TestGetWithRetrySucceedsAfterTransientErrors(t *testing.T) {
	withFastBackoff(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Fail with a retryable status the first two times, then succeed.
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	resp, err := getWithRetry(context.Background(), srv.Client(), func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("getWithRetry returned error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %s, want 200 after retries", resp.Status)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("server called %d times, want 3 (two transient failures then success)", got)
	}
}

func TestGetWithRetryDoesNotRetryClientError(t *testing.T) {
	withFastBackoff(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	resp, err := getWithRetry(context.Background(), srv.Client(), func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("getWithRetry returned error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %s, want 404 returned to caller", resp.Status)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server called %d times, want 1 (404 is not retried)", got)
	}
}

func TestGetWithRetryExhaustsAttempts(t *testing.T) {
	withFastBackoff(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	// Every attempt is a retryable 502; the last one is returned to the caller
	// (not an error) so it can report the real status.
	resp, err := getWithRetry(context.Background(), srv.Client(), func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("getWithRetry returned error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %s, want 502 on the final attempt", resp.Status)
	}
	if got := calls.Load(); got != int32(httpMaxAttempts) {
		t.Errorf("server called %d times, want %d", got, httpMaxAttempts)
	}
}

func TestGetWithRetryHonorsContextCancellation(t *testing.T) {
	// Keep the real (longer) backoff so the cancellation wins the race.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled up front: the first backoff must abort.

	_, err := getWithRetry(ctx, srv.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err == nil {
		t.Fatal("expected an error when the context is cancelled")
	}
}
