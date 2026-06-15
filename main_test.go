package main

import "testing"

// resetDebounceState clears the global debounce state between test cases.
func resetDebounceState(threshold int) {
	failureStreaks = map[string]int{}
	conf.FailureThreshold = threshold
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

	// A blip that appears for one cycle and then recovers must never be confirmed.
	if got := confirmedMessages(issueEntries{issue("yivi.app: cannot be reached")}); len(got) != 0 {
		t.Fatalf("cycle 1: expected nothing confirmed, got %v", got)
	}
	if got := confirmedMessages(issueEntries{}); len(got) != 0 {
		t.Fatalf("cycle 2 (recovered): expected nothing confirmed, got %v", got)
	}
	// Streak must have been reset, so the issue counts from scratch if it returns.
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
	// Third consecutive cycle: the threshold is reached and the issue is confirmed.
	got := confirmedMessages(issueEntries{issue(msg)})
	if len(got) != 1 || got[0] != msg {
		t.Fatalf("cycle 3: expected %q confirmed, got %v", msg, got)
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

// TestDebounceEndToEnd mirrors the runChecks reporting loop: an issue should be
// reported as "new" only once confirmed, and a transient blip should produce no
// new/fixed churn at all.
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

	// Cycle 1: blip detected but not yet confirmed -> no alert.
	if n, f := step(issueEntries{issue(blip)}); len(n) != 0 || len(f) != 0 {
		t.Fatalf("cycle 1: expected no churn, got new=%v fixed=%v", n, f)
	}
	// Cycle 2: blip recovered -> still no alert and nothing to mark fixed.
	if n, f := step(issueEntries{}); len(n) != 0 || len(f) != 0 {
		t.Fatalf("cycle 2: expected no churn, got new=%v fixed=%v", n, f)
	}

	// Now a genuinely persistent issue across two cycles.
	persistent := "keyshare.yivi.app: cannot be reached"
	if n, _ := step(issueEntries{issue(persistent)}); len(n) != 0 {
		t.Fatalf("cycle 3: expected not yet reported, got new=%v", n)
	}
	n, _ := step(issueEntries{issue(persistent)})
	if len(n) != 1 || n[0].message != persistent {
		t.Fatalf("cycle 4: expected %q reported as new, got %v", persistent, n.messages())
	}
	// Recovery is reported as fixed exactly once.
	_, f := step(issueEntries{})
	if len(f) != 1 || f[0].message != persistent {
		t.Fatalf("cycle 5: expected %q reported as fixed, got %v", persistent, f.messages())
	}
}
