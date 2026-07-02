package main

import (
	"fmt"
	"net/url"
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
