package main

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

// maxLoggedBodyLen bounds how much of a monitored response body we write to the
// logs. Response bodies are attacker- or third-party-controlled and can be large
// or contain sensitive data, so we only ever log a short prefix.
const maxLoggedBodyLen = 512

// truncateForLog returns a bounded, single-line-friendly version of s suitable
// for logging. Anything beyond maxLoggedBodyLen is dropped and replaced with a
// note stating how many bytes were omitted.
func truncateForLog(s string) string {
	if len(s) <= maxLoggedBodyLen {
		return s
	}
	return s[:maxLoggedBodyLen] + fmt.Sprintf("… (%d more bytes truncated)", len(s)-maxLoggedBodyLen)
}

// redactURL returns a form of raw that is safe to log: the scheme and host are
// kept for debugging, but the path, query and any userinfo — which for webhook
// URLs are the secret credential — are replaced with a redaction marker.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[redacted]"
	}
	return u.Scheme + "://" + u.Host + "/[redacted]"
}

// redactErr returns err's message with every occurrence of rawURL replaced by
// its redacted form. Network failures surface the request URL inside the error
// itself — the stdlib returns a *url.Error whose message is `Get "<url>": ...`
// and the Slack client (gorequest) wraps net/http similarly — so logging err
// verbatim leaks the secret webhook token even when the accompanying label is
// already redacted. Callers must pass the exact URL string they handed to the
// HTTP call so it can be matched and stripped.
func redactErr(err error, rawURL string) string {
	if err == nil {
		return ""
	}
	return redactURLIn(err.Error(), rawURL)
}

// redactErrs is the []error counterpart of redactErr, used for the Slack client
// whose Send returns a slice. Each error is sanitized and joined, so a leaked
// URL in any element is stripped.
func redactErrs(errs []error, rawURL string) string {
	msgs := make([]string, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			msgs = append(msgs, redactURLIn(err.Error(), rawURL))
		}
	}
	return strings.Join(msgs, "; ")
}

// redactURLIn replaces every occurrence of rawURL in s with its redacted form.
func redactURLIn(s, rawURL string) string {
	return strings.ReplaceAll(s, rawURL, redactURL(rawURL))
}

func newHTTPClient() *retryablehttp.Client {
	// Use retryablehttp to prevent false positives.
	client := retryablehttp.NewClient()
	client.HTTPClient.Timeout = 5 * time.Second
	client.RetryMax = 3
	client.RetryWaitMin = 500 * time.Millisecond
	client.RetryWaitMax = 1 * time.Second

	// Disable debug logging
	client.Logger = nil

	return client
}
