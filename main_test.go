package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestSendWebHookReusesConnection verifies that sendWebHook drains and closes
// the response body, so the underlying TCP connection is returned to the pool
// and reused instead of leaked on every alert.
func TestSendWebHookReusesConnection(t *testing.T) {
	var newConns int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt32(&newConns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	for i := 0; i < 5; i++ {
		if !sendWebHook(srv.URL) {
			t.Fatalf("sendWebHook returned false on request %d", i)
		}
	}

	// If the body were left open, each request would open a fresh connection.
	if got := atomic.LoadInt32(&newConns); got != 1 {
		t.Errorf("expected connection reuse (1 connection), got %d — response body likely not closed", got)
	}
}

// TestSendWebHookRespectsTimeout verifies that a slow endpoint can't block the
// delivery goroutine indefinitely.
func TestSendWebHookRespectsTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	// Unblock the handler before closing so srv.Close doesn't wait forever.
	defer srv.Close()
	defer close(block)

	old := webHookClient.Timeout
	webHookClient.Timeout = 100 * time.Millisecond
	defer func() { webHookClient.Timeout = old }()

	start := time.Now()
	if sendWebHook(srv.URL) {
		t.Fatal("expected sendWebHook to fail against a hanging endpoint")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("sendWebHook did not respect timeout, took %s", elapsed)
	}
}
