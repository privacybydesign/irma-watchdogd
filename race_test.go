package main

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestHandlerStateRace exercises the HTTP handler concurrently with the same
// global-state mutation runChecks performs at the end of a cycle. Run with
// -race: it reproduces the unsynchronised access to the shared globals.
func TestHandlerStateRace(t *testing.T) {
	var err error
	parsedTemplate, err = template.New("template").Parse(rawTemplate)
	if err != nil {
		t.Fatalf("parse template: %s", err)
	}
	conf.Interval = 5 * time.Minute

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: mimic the tail of runChecks updating the shared globals.
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			setState(issueEntries{{warning, "x"}}, time.Now())
		}
	})

	// Readers: hit the handler the way an HTTP client would.
	for range 4 {
		wg.Go(func() {
			for range 200 {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				handler(rec, req)
			}
		})
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
