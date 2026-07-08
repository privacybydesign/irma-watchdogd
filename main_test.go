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

// resetDebounceState clears the global debounce state between test cases.
func resetDebounceState(threshold int) {
	failureStreaks = map[string]int{}
	recoveryStreaks = map[string]int{}
	confirmedSet = map[string]issueEntry{}
	conf.FailureThreshold = threshold
	cycleCount = 0
	initialCheck = false
}

func issue(msg string) issueEntry {
	return issueEntry{danger, msg}
}

// confirmedMessages runs one debounce cycle and returns the confirmed messages.
func confirmedMessages(cur issueEntries) []string {
	return confirmIssues(cur).messages()
}

func TestConfirmIssuesSuppressesSingleCycleBlip(t *testing.T) {
	resetDebounceState(3)

	if got := confirmedMessages(issueEntries{issue("yivi.app: cannot be reached")}); len(got) != 0 {
		t.Fatalf("cycle 1: expected nothing confirmed, got %v", got)
	}
	if got := confirmedMessages(issueEntries{}); len(got) != 0 {
		t.Fatalf("cycle 2 (recovered): expected nothing confirmed, got %v", got)
	}
	// Streak reset, so the issue counts from scratch on return.
	if got := confirmedMessages(issueEntries{issue("yivi.app: cannot be reached")}); len(got) != 0 {
		t.Fatalf("cycle 3 (returned): expected nothing confirmed, got %v", got)
	}
}

func TestConfirmIssuesReportsPersistentIssue(t *testing.T) {
	resetDebounceState(3)

	msg := "privacybydesign.foundation: cannot be reached"
	for cycle := 1; cycle <= 2; cycle++ {
		if got := confirmedMessages(issueEntries{issue(msg)}); len(got) != 0 {
			t.Fatalf("cycle %d: expected not yet confirmed, got %v", cycle, got)
		}
	}
	// Threshold reached on the third consecutive cycle.
	got := confirmedMessages(issueEntries{issue(msg)})
	if len(got) != 1 || got[0] != msg {
		t.Fatalf("cycle 3: expected %q confirmed, got %v", msg, got)
	}
}

func TestConfirmIssuesDeduplicatesMessagesWithinCycle(t *testing.T) {
	resetDebounceState(3)

	msg := "yivi.app: cannot be reached"
	// Duplicate entries in one cycle must advance the streak by one, not two.
	dup := issueEntries{issue(msg), issue(msg)}

	for cycle := 1; cycle <= 2; cycle++ {
		if got := confirmedMessages(dup); len(got) != 0 {
			t.Fatalf("cycle %d: expected not yet confirmed, got %v", cycle, got)
		}
	}
	got := confirmedMessages(dup)
	if len(got) != 1 || got[0] != msg {
		t.Fatalf("cycle 3: expected %q confirmed exactly once, got %v", msg, got)
	}
}

func TestConfirmIssuesThresholdOneAlertsImmediately(t *testing.T) {
	resetDebounceState(1)

	msg := "schemes.yivi.app: cannot be reached"
	got := confirmedMessages(issueEntries{issue(msg)})
	if len(got) != 1 || got[0] != msg {
		t.Fatalf("threshold 1: expected immediate confirmation, got %v", got)
	}
}

// TestDebounceEndToEnd mirrors the runChecks reporting loop.
func TestDebounceEndToEnd(t *testing.T) {
	resetDebounceState(2)

	var reported issueEntries // plays the role of the global `issues`
	step := func(cur issueEntries) (newIssues, fixedIssues issueEntries) {
		confirmed := confirmIssues(cur)
		newIssues, fixedIssues = difference(reported, confirmed)
		reported = confirmed
		return
	}

	blip := "yivi.app: cannot be reached"

	// Blip: detected then recovered within the threshold -> no churn.
	if n, f := step(issueEntries{issue(blip)}); len(n) != 0 || len(f) != 0 {
		t.Fatalf("cycle 1: expected no churn, got new=%v fixed=%v", n, f)
	}
	if n, f := step(issueEntries{}); len(n) != 0 || len(f) != 0 {
		t.Fatalf("cycle 2: expected no churn, got new=%v fixed=%v", n, f)
	}

	// Persistent issue: reported new only once confirmed, fixed only once absent
	// for the threshold.
	persistent := "keyshare.yivi.app: cannot be reached"
	if n, _ := step(issueEntries{issue(persistent)}); len(n) != 0 {
		t.Fatalf("cycle 3: expected not yet reported, got new=%v", n)
	}
	n, _ := step(issueEntries{issue(persistent)})
	if len(n) != 1 || n[0].message != persistent {
		t.Fatalf("cycle 4: expected %q reported as new, got %v", persistent, n.messages())
	}
	if n, f := step(issueEntries{}); len(n) != 0 || len(f) != 0 {
		t.Fatalf("cycle 5: expected no churn on first absent cycle, got new=%v fixed=%v", n.messages(), f.messages())
	}
	_, f := step(issueEntries{})
	if len(f) != 1 || f[0].message != persistent {
		t.Fatalf("cycle 6: expected %q reported as fixed, got %v", persistent, f.messages())
	}
}

// TestConfirmIssuesDebouncesSingleCycleRecoveryBlip: a confirmed issue that
// flaps to OK for a single cycle stays confirmed.
func TestConfirmIssuesDebouncesSingleCycleRecoveryBlip(t *testing.T) {
	resetDebounceState(3)

	msg := "keyshare.yivi.app: cannot be reached"
	present := issueEntries{issue(msg)}

	for cycle := 1; cycle <= 3; cycle++ {
		confirmedMessages(present)
	}
	if _, ok := confirmedSet[msg]; !ok {
		t.Fatalf("expected %q to be confirmed after 3 cycles", msg)
	}

	// A single absent cycle (a recovery blip) must keep the issue confirmed.
	got := confirmedMessages(issueEntries{})
	if len(got) != 1 || got[0] != msg {
		t.Fatalf("recovery blip: expected %q to remain confirmed, got %v", msg, got)
	}

	// On return the recovery streak resets.
	got = confirmedMessages(present)
	if len(got) != 1 || got[0] != msg {
		t.Fatalf("returned: expected %q to remain confirmed, got %v", msg, got)
	}
	if _, ok := recoveryStreaks[msg]; ok {
		t.Fatalf("expected recovery streak to reset once the issue returned")
	}
}

// TestConfirmIssuesReportsFixedAfterThreshold: a confirmed issue is dropped only
// once absent for FailureThreshold consecutive cycles.
func TestConfirmIssuesReportsFixedAfterThreshold(t *testing.T) {
	resetDebounceState(3)

	msg := "privacybydesign.foundation: cannot be reached"
	present := issueEntries{issue(msg)}

	for cycle := 1; cycle <= 3; cycle++ {
		confirmedMessages(present)
	}

	// Absent cycles 1 and 2 (< threshold): the issue is still confirmed.
	for cycle := 1; cycle <= 2; cycle++ {
		got := confirmedMessages(issueEntries{})
		if len(got) != 1 || got[0] != msg {
			t.Fatalf("absent cycle %d: expected %q still confirmed, got %v", cycle, msg, got)
		}
	}

	// Absent cycle 3: the recovery threshold is reached and the issue is dropped.
	got := confirmedMessages(issueEntries{})
	if len(got) != 0 {
		t.Fatalf("absent cycle 3: expected %q dropped from confirmed set, got %v", msg, got)
	}
	if _, ok := failureStreaks[msg]; ok {
		t.Fatalf("expected failure streak cleaned up after fixed confirmation")
	}
	if _, ok := recoveryStreaks[msg]; ok {
		t.Fatalf("expected recovery streak cleaned up after fixed confirmation")
	}
}

// stepInitialWindow mirrors the initialCheck bookkeeping in runChecks.
func stepInitialWindow() bool {
	cycleCount++
	initialCheck = cycleCount <= conf.FailureThreshold
	return initialCheck
}

// TestInitialCheckWindowCoversDebounceDelay: the initialCheck window must stay
// open until a startup-present issue is confirmed (on cycle FailureThreshold),
// else a restart-time outage would page as brand new.
func TestInitialCheckWindowCoversDebounceDelay(t *testing.T) {
	resetDebounceState(3)

	startupIssue := "yivi.app: cannot be reached"

	// Cycles 1 and 2: detected but not yet confirmed; window stays open.
	for cycle := 1; cycle <= 2; cycle++ {
		initial := stepInitialWindow()
		if got := confirmedMessages(issueEntries{issue(startupIssue)}); len(got) != 0 {
			t.Fatalf("cycle %d: expected not yet confirmed, got %v", cycle, got)
		}
		if !initial {
			t.Fatalf("cycle %d: expected initialCheck to still be true", cycle)
		}
	}

	// Cycle 3: startup issue first confirmed, still flagged initial.
	initial := stepInitialWindow()
	got := confirmedMessages(issueEntries{issue(startupIssue)})
	if len(got) != 1 || got[0] != startupIssue {
		t.Fatalf("cycle 3: expected %q confirmed, got %v", startupIssue, got)
	}
	if !initial {
		t.Fatalf("cycle 3: expected initialCheck to be true when the startup issue is first confirmed")
	}

	// Cycle 4: the startup window is closed.
	if initial := stepInitialWindow(); initial {
		t.Fatalf("cycle 4: expected initialCheck to be false once the startup window has passed")
	}
}

// TestInitialCheckThresholdOneMatchesOldBehaviour: with FailureThreshold 1 only
// the first cycle is initial.
func TestInitialCheckThresholdOneMatchesOldBehaviour(t *testing.T) {
	resetDebounceState(1)

	if initial := stepInitialWindow(); !initial {
		t.Fatalf("cycle 1: expected initialCheck to be true")
	}
	if initial := stepInitialWindow(); initial {
		t.Fatalf("cycle 2: expected initialCheck to be false")
	}
}
