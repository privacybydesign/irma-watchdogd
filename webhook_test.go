package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestPushToWebHooksContinuesOnError verifies that a failing webhook endpoint
// does not abort delivery to the remaining endpoints or subsequent alerts
// (issue #36: the loop previously returned on the first error).
func TestPushToWebHooksContinuesOnError(t *testing.T) {
	var hits int32
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer good.Close()

	// A server that is immediately closed: requests to it fail with
	// "connection refused", simulating an unreachable endpoint.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	badURL := bad.URL
	bad.Close()

	oldConf := conf
	defer func() { conf = oldConf }()
	// The failing endpoint is listed first; if the loop aborts on its error the
	// healthy endpoint below is never reached.
	conf = Conf{WebHooks: []string{badURL + "/?m=%s", good.URL + "/?m=%s"}}

	pushToWebHooks(issueEntries{{issueType: danger, message: "boom"}})

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected the healthy webhook to be hit once despite the failing one, got %d", got)
	}
}

// TestPushToWebHooksDeliversAllDangers verifies every danger-level alert is
// delivered to every configured endpoint.
func TestPushToWebHooksDeliversAllDangers(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	oldConf := conf
	defer func() { conf = oldConf }()
	conf = Conf{WebHooks: []string{srv.URL + "/?m=%s", srv.URL + "/?m=%s"}}

	pushToWebHooks(issueEntries{
		{issueType: danger, message: "one"},
		{issueType: warning, message: "ignored"}, // warnings are filtered out
		{issueType: danger, message: "two"},
	})

	// 2 dangers x 2 endpoints = 4 deliveries; the warning is not sent.
	if got := atomic.LoadInt32(&hits); got != 4 {
		t.Fatalf("expected 4 webhook deliveries (2 dangers x 2 endpoints), got %d", got)
	}
}
