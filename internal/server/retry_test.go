package server

import (
	"context"
	"errors"
	"testing"
	"time"

	deepseek "github.com/cohesion-org/deepseek-go"
)

// flakyClient returns the given errors on the first len(errs) calls,
// then yields successResp on every subsequent call. Counts calls so
// tests can assert the retry policy invoked the upstream the expected
// number of times.
type flakyClient struct {
	errs        []error
	successResp *deepseek.ChatCompletionResponse
	calls       int
}

func (f *flakyClient) CreateChatCompletion(ctx context.Context, req *deepseek.ChatCompletionRequest) (*deepseek.ChatCompletionResponse, error) {
	f.calls++
	if f.calls <= len(f.errs) {
		return nil, f.errs[f.calls-1]
	}
	return f.successResp, nil
}

func apiErr(status int) error {
	return &deepseek.APIError{StatusCode: status, Message: "test"}
}

// newFastRetryServer overrides retry waits so tests don't sleep for
// real-world seconds. Backoff math is still exercised; just at micro scale.
func newFastRetryServer(c DeepSeekClient) *Server {
	s := New(c)
	s.retryBaseWait = time.Microsecond
	s.retryMaxWait = 10 * time.Microsecond
	return s
}

func TestRetry_429RetriesThenSucceeds(t *testing.T) {
	fc := &flakyClient{
		errs:        []error{apiErr(429), apiErr(429)},
		successResp: makeResp("recovered", ""),
	}
	s := newFastRetryServer(fc)

	_, out, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hi"})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if out.Content != "recovered" {
		t.Errorf("content = %q, want 'recovered'", out.Content)
	}
	if fc.calls != 3 {
		t.Errorf("calls = %d, want 3 (two 429s, one success)", fc.calls)
	}
}

func TestRetry_5xxRetries(t *testing.T) {
	fc := &flakyClient{
		errs:        []error{apiErr(503)},
		successResp: makeResp("ok", ""),
	}
	s := newFastRetryServer(fc)

	if _, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hi"}); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if fc.calls != 2 {
		t.Errorf("calls = %d, want 2", fc.calls)
	}
}

func TestRetry_4xxNotRetried(t *testing.T) {
	// 401, 400, 404 etc. are caller bugs — retrying just wastes time.
	for _, code := range []int{400, 401, 403, 404} {
		fc := &flakyClient{
			errs:        []error{apiErr(code)},
			successResp: makeResp("never reached", ""),
		}
		s := newFastRetryServer(fc)

		_, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hi"})
		if err == nil {
			t.Errorf("status %d: expected error to surface immediately", code)
		}
		if fc.calls != 1 {
			t.Errorf("status %d: calls = %d, want 1 (no retry)", code, fc.calls)
		}
	}
}

func TestRetry_ExhaustsRetriesAndSurfacesError(t *testing.T) {
	// More 429s than the retry budget allows.
	fc := &flakyClient{
		errs: []error{apiErr(429), apiErr(429), apiErr(429), apiErr(429), apiErr(429), apiErr(429)},
	}
	s := newFastRetryServer(fc)

	_, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error after retry exhaustion")
	}
	if fc.calls != s.retryMax+1 {
		t.Errorf("calls = %d, want %d (retryMax+1)", fc.calls, s.retryMax+1)
	}
	var apiErr *deepseek.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 {
		t.Errorf("expected wrapped APIError with status 429, got %v", err)
	}
}

func TestRetry_ContextCancellationStopsRetries(t *testing.T) {
	fc := &flakyClient{
		errs: []error{apiErr(429), apiErr(429), apiErr(429), apiErr(429), apiErr(429)},
	}
	s := New(fc) // default retry waits — long enough that ctx cancel wins
	s.retryBaseWait = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	if _, _, err := s.ConsultOneshot(ctx, nil, OneshotInput{Prompt: "hi"}); err == nil {
		t.Fatal("expected error from context cancellation")
	}
	if fc.calls >= s.retryMax+1 {
		t.Errorf("calls = %d, expected ctx cancel to stop retries early", fc.calls)
	}
}

func TestRetry_NonAPIErrorNotRetried(t *testing.T) {
	// Network errors, marshaling errors, etc. that aren't *deepseek.APIError
	// are treated as final to avoid masking bugs.
	fc := &flakyClient{
		errs: []error{errors.New("connection refused")},
	}
	s := newFastRetryServer(fc)

	if _, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hi"}); err == nil {
		t.Fatal("expected error to surface")
	}
	if fc.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on non-API error)", fc.calls)
	}
}

func TestBackoffDelay_ScalesAndCaps(t *testing.T) {
	base := 100 * time.Millisecond
	maxD := 500 * time.Millisecond
	// Each call returns a value in [0, computed]; sample many to bound the max.
	for attempt := 1; attempt <= 10; attempt++ {
		seen := time.Duration(0)
		for range 50 {
			d := backoffDelay(base, maxD, attempt)
			if d > seen {
				seen = d
			}
		}
		if seen > maxD {
			t.Errorf("attempt %d: max sampled delay %v exceeds cap %v", attempt, seen, maxD)
		}
		if attempt >= 4 && seen == 0 {
			t.Errorf("attempt %d: never sampled non-zero delay (jitter broken?)", attempt)
		}
	}
}
