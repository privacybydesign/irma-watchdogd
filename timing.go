package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/http/httptrace"
	"sync"
	"time"
)

// requestTrace captures the connection-phase timings of a single HTTP attempt
// using net/http/httptrace. When a check fails or is slow we can then see which
// phase (DNS resolution, TCP connect, TLS handshake, or waiting for the first
// response byte) was responsible — measured from the watchdog's own vantage
// point, which is what we cannot observe from a browser on a workstation.
type requestTrace struct {
	mu sync.Mutex

	attempt int

	start     time.Time
	dnsStart  time.Time
	dnsDone   time.Time
	connStart time.Time
	connDone  time.Time
	tlsStart  time.Time
	tlsDone   time.Time
	firstByte time.Time
	reused    bool
}

func newRequestTrace() *requestTrace {
	return &requestTrace{start: time.Now()}
}

// clientTrace returns an httptrace.ClientTrace that records phase timestamps
// into t. Attach it to a request context with httptrace.WithClientTrace.
func (t *requestTrace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { t.mark(&t.dnsStart) },
		DNSDone:              func(httptrace.DNSDoneInfo) { t.mark(&t.dnsDone) },
		ConnectStart:         func(_, _ string) { t.mark(&t.connStart) },
		ConnectDone:          func(_, _ string, _ error) { t.mark(&t.connDone) },
		TLSHandshakeStart:    func() { t.mark(&t.tlsStart) },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { t.mark(&t.tlsDone) },
		GotFirstResponseByte: func() { t.mark(&t.firstByte) },
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			t.reused = info.Reused
			t.mu.Unlock()
		},
	}
}

// mark records the current time into field, but only the first time it fires
// within an attempt (some phases, e.g. connect on dual-stack hosts, fire twice).
func (t *requestTrace) mark(field *time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if field.IsZero() {
		*field = time.Now()
	}
}

// reset clears the timings at the start of each attempt. retryablehttp reuses
// the same request (and therefore the same trace) across retries, so without
// this the timings of a later attempt would be masked by an earlier one.
func (t *requestTrace) reset(attempt int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.attempt = attempt
	t.start = time.Now()
	t.dnsStart, t.dnsDone = time.Time{}, time.Time{}
	t.connStart, t.connDone = time.Time{}, time.Time{}
	t.tlsStart, t.tlsDone = time.Time{}, time.Time{}
	t.firstByte = time.Time{}
	t.reused = false
}

// summary renders a one-line, human-readable breakdown of the attempt. Phases
// that did not complete are shown as "-", which itself is diagnostic: a hung TLS
// handshake shows "tls=-", a DNS timeout shows everything after dns as "-".
func (t *requestTrace) summary() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	phase := func(start, end time.Time) string {
		if start.IsZero() || end.IsZero() {
			return "-"
		}
		return end.Sub(start).Round(time.Millisecond).String()
	}

	ttfb := "-"
	if !t.firstByte.IsZero() {
		ttfb = t.firstByte.Sub(t.start).Round(time.Millisecond).String()
	}

	return fmt.Sprintf("dns=%s connect=%s tls=%s ttfb=%s total=%s reused=%t",
		phase(t.dnsStart, t.dnsDone),
		phase(t.connStart, t.connDone),
		phase(t.tlsStart, t.tlsDone),
		ttfb,
		time.Since(t.start).Round(time.Millisecond),
		t.reused,
	)
}

// logFailedAttempt emits the phase breakdown for an attempt that errored or was
// otherwise unhealthy. Healthy attempts are not logged, so normal cycles stay
// quiet and the only trace output is for the events we want to diagnose.
func logFailedAttempt(method, url string, t *requestTrace, err error) {
	if err != nil {
		log.Printf("trace %s %s attempt %d failed (%s) err=%s", method, url, t.attempt, t.summary(), err)
	} else {
		log.Printf("trace %s %s attempt %d unhealthy (%s)", method, url, t.attempt, t.summary())
	}
}
