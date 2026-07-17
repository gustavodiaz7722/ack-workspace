package scanner

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// httpMaxAttempts is the total number of times a request is issued before
// giving up (the first try plus retries). GitHub's raw and API endpoints
// occasionally return a transient 5xx or throttle with 429; a few retries with
// backoff smooth those over without burning agent conversation turns.
const httpMaxAttempts = 4

// httpRetryBackoff is the base delay for the exponential backoff between
// attempts (base, 2x, 4x, ...). It is a var, not a const, so tests can shrink it
// to keep retry paths fast.
var httpRetryBackoff = 300 * time.Millisecond

// isRetryableStatus reports whether an HTTP status warrants a retry: rate
// limiting (429) or a server-side error (5xx). Client errors such as a 404 for
// an unknown slug or model are not retried — they will not succeed on retry.
func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// getWithRetry issues the request built by newReq with bounded exponential
// backoff, retrying transport errors and retryable statuses (429/5xx). A fresh
// request is built for each attempt because a request cannot be safely reused
// across Do calls.
//
// On the final outcome it returns the response with its body still open for the
// caller to read and close (including a non-retryable non-200, so the caller can
// inspect the status). It returns the last error only when every attempt fails
// at the transport level or the context is cancelled during backoff.
func getWithRetry(ctx context.Context, client *http.Client, newReq func() (*http.Request, error)) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= httpMaxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(httpRetryBackoff * (1 << (attempt - 2))):
			}
		}

		req, err := newReq()
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		// Retry a transient status only while attempts remain; on the last
		// attempt return the response so the caller reports the real status.
		if isRetryableStatus(resp.StatusCode) && attempt < httpMaxAttempts {
			lastErr = fmt.Errorf("unexpected status %s", resp.Status)
			resp.Body.Close()
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}
