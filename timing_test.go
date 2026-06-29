package main

import (
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

// TestRequestTraceCapturesPhases runs a real request through the same wiring the
// checks use and confirms the connection phases are recorded.
func TestRequestTraceCapturesPhases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newHTTPClient()
	req, err := retryablehttp.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	trace := newRequestTrace()
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace.clientTrace()))
	client.RequestLogHook = func(_ retryablehttp.Logger, _ *http.Request, attempt int) {
		trace.reset(attempt)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	s := trace.summary()
	if strings.Contains(s, "connect=-") {
		t.Errorf("expected TCP connect phase to be recorded, got %q", s)
	}
	if strings.Contains(s, "ttfb=-") {
		t.Errorf("expected time-to-first-byte to be recorded, got %q", s)
	}
}

// TestRequestTraceSummaryShowsIncompletePhases is the diagnostic payoff: a phase
// that never completes must render as "-" so the log pinpoints where it hung.
func TestRequestTraceSummaryShowsIncompletePhases(t *testing.T) {
	now := time.Now()
	tr := &requestTrace{
		start:     now,
		connStart: now,
		connDone:  now.Add(10 * time.Millisecond),
		tlsStart:  now.Add(10 * time.Millisecond),
		// tlsDone left zero -> handshake hung; firstByte zero -> no response
	}

	s := tr.summary()
	if !strings.Contains(s, "tls=-") {
		t.Errorf("expected tls=- for an incomplete handshake, got %q", s)
	}
	if !strings.Contains(s, "ttfb=-") {
		t.Errorf("expected ttfb=- when no first byte arrived, got %q", s)
	}
	if strings.Contains(s, "connect=-") {
		t.Errorf("expected the completed connect phase to show a duration, got %q", s)
	}
}

func TestRequestTraceResetClearsPhases(t *testing.T) {
	tr := newRequestTrace()
	tr.mark(&tr.dnsStart)
	tr.mark(&tr.connStart)

	tr.reset(2)

	if tr.attempt != 2 {
		t.Errorf("attempt = %d, want 2", tr.attempt)
	}
	if !tr.dnsStart.IsZero() || !tr.connStart.IsZero() {
		t.Errorf("reset did not clear phase timestamps: %s", tr.summary())
	}
}
