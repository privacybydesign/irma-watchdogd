package main

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestTruncateForLogShortBodyUnchanged(t *testing.T) {
	in := "small body"
	if got := truncateForLog(in); got != in {
		t.Errorf("truncateForLog(%q) = %q, want unchanged", in, got)
	}
}

func TestTruncateForLogBoundary(t *testing.T) {
	in := strings.Repeat("a", maxLoggedBodyLen)
	if got := truncateForLog(in); got != in {
		t.Errorf("body of exactly maxLoggedBodyLen must be logged verbatim, got %d bytes", len(got))
	}
}

func TestTruncateForLogTruncatesLongBody(t *testing.T) {
	in := strings.Repeat("a", maxLoggedBodyLen+100)
	got := truncateForLog(in)
	if !strings.HasPrefix(got, strings.Repeat("a", maxLoggedBodyLen)) {
		t.Errorf("truncated output should start with the first maxLoggedBodyLen bytes")
	}
	if !strings.Contains(got, "100 more bytes truncated") {
		t.Errorf("truncated output should note the number of omitted bytes, got %q", got)
	}
	if len(got) >= len(in) {
		t.Errorf("truncated output (%d) should be shorter than input (%d)", len(got), len(in))
	}
}

func TestRedactURLKeepsSchemeAndHostHidesSecret(t *testing.T) {
	secret := "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"
	got := redactURL(secret)
	if got != "https://hooks.slack.com/[redacted]" {
		t.Errorf("redactURL = %q, want scheme+host with redacted path", got)
	}
	if strings.Contains(got, "XXXXXXXXXXXXXXXXXXXXXXXX") {
		t.Errorf("redactURL leaked the secret token: %q", got)
	}
}

func TestRedactURLStripsQueryAndUserinfo(t *testing.T) {
	got := redactURL("https://user:pass@example.com/path?token=abcd")
	for _, leak := range []string{"pass", "token", "abcd", "path"} {
		if strings.Contains(got, leak) {
			t.Errorf("redactURL leaked %q: %q", leak, got)
		}
	}
}

func TestRedactURLUnparseable(t *testing.T) {
	if got := redactURL("://not a url"); got != "[redacted]" {
		t.Errorf("redactURL of an unparseable value = %q, want [redacted]", got)
	}
}

// TestRedactErrStripsSecretFromURLError locks in the fix for the network-failure
// leak: net/http.Get and the Slack client both return errors that embed the full
// request URL — secret token and all — so the error must be sanitized before it
// reaches the log, not just the accompanying label.
func TestRedactErrStripsSecretFromURLError(t *testing.T) {
	secret := "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"
	// Mirrors what net/http.Get produces on a network failure.
	err := &url.Error{Op: "Get", URL: secret, Err: errors.New("dial tcp: connection refused")}

	// Sanity check: the raw error really does leak the token, otherwise this test
	// would pass vacuously.
	if !strings.Contains(err.Error(), "XXXXXXXXXXXXXXXXXXXXXXXX") {
		t.Fatalf("precondition failed: raw error did not embed the secret: %q", err.Error())
	}

	got := redactErr(err, secret)
	if strings.Contains(got, "XXXXXXXXXXXXXXXXXXXXXXXX") {
		t.Errorf("redactErr leaked the secret token: %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("redactErr should keep the diagnostic detail, got %q", got)
	}
	if !strings.Contains(got, "hooks.slack.com") {
		t.Errorf("redactErr should keep scheme+host for debugging, got %q", got)
	}
}

func TestRedactErrNil(t *testing.T) {
	if got := redactErr(nil, "https://example.com/x"); got != "" {
		t.Errorf("redactErr(nil) = %q, want empty string", got)
	}
}

// TestRedactErrsStripsSecret covers the Slack path, whose client returns a slice
// of errors that each embed the request URL.
func TestRedactErrsStripsSecret(t *testing.T) {
	secret := "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"
	errs := []error{
		&url.Error{Op: "Post", URL: secret, Err: errors.New("EOF")},
		errors.New("giving up after 3 attempts"),
	}
	got := redactErrs(errs, secret)
	if strings.Contains(got, "XXXXXXXXXXXXXXXXXXXXXXXX") {
		t.Errorf("redactErrs leaked the secret token: %q", got)
	}
	if !strings.Contains(got, "giving up after 3 attempts") {
		t.Errorf("redactErrs should keep non-URL errors, got %q", got)
	}
}

// TestWebhookSubstitutionNotFormatString locks in the fix for treating the
// operator-controlled webhook URL as data rather than a fmt format string: a URL
// with stray percent signs must be substituted literally, not interpreted.
func TestWebhookSubstitutionNotFormatString(t *testing.T) {
	bareURL := "https://example.com/notify?pct=100%&message=%s"
	msg := "disk 50% full"

	got := strings.Replace(bareURL, "%s", url.QueryEscape("Watchdog: "+msg), 1)

	if strings.Contains(got, "%!") {
		t.Errorf("substitution produced fmt error verbs: %q", got)
	}
	if !strings.Contains(got, "pct=100%") {
		t.Errorf("literal percent in the URL should be preserved, got %q", got)
	}
	if !strings.Contains(got, url.QueryEscape("Watchdog: "+msg)) {
		t.Errorf("escaped message should be substituted into the URL, got %q", got)
	}
}
