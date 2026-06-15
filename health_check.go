package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"strings"

	"github.com/hashicorp/go-retryablehttp"
)

type HealthCheck struct {
	RequestURL     string
	RequestMethod  string // Defaults to "GET"
	RequestHeaders map[string]string
	RequestBody    string

	ResponseStatusCodeEquals int // Defaults to 200
	ResponseHeaderContains   map[string]string
	ResponseBodyContains     string
}

func runHealthChecks(checks []HealthCheck) (issues issueEntries) {
	// Re-use the same HTTP client for all checks to prevent false positives due to DNS resolution, TLS handshake issues or connection starvastion
	client := newHTTPClient()
	issueChan := make(chan *issueEntry, len(checks))

	for _, check := range checks {
		check := check
		issueChan <- runHealthCheck(client, check)
	}

	close(issueChan)

	for issue := range issueChan {
		if issue != nil {
			issues = append(issues, *issue)
		}
	}
	return
}

func runHealthCheck(client *retryablehttp.Client, check HealthCheck) *issueEntry {
	log.Printf(" checking HTTP endpoint %s", check.RequestURL)

	// Set defaults
	if check.RequestMethod == "" {
		check.RequestMethod = "GET"
	}
	if check.ResponseStatusCodeEquals == 0 {
		check.ResponseStatusCodeEquals = 200
	}

	// Use retryablehttp to prevent false positives.
	req, err := retryablehttp.NewRequest(check.RequestMethod, check.RequestURL, []byte(check.RequestBody))
	if err != nil {
		log.Printf("Health check %s: %s", check.RequestURL, err)
		return &issueEntry{warning, fmt.Sprintf("%s: invalid health check", check.RequestURL)}
	}
	for key, value := range check.RequestHeaders {
		req.Header.Set(key, value)
	}

	// Record per-attempt connection timings so a failure tells us which phase
	// (DNS, connect, TLS, first byte) was slow or hung, from the pod's vantage.
	trace := newRequestTrace()
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace.clientTrace()))
	client.RequestLogHook = func(_ retryablehttp.Logger, _ *http.Request, attempt int) {
		trace.reset(attempt)
	}

	var issue *issueEntry

	client.CheckRetry = func(ctx context.Context, resp *http.Response, respErr error) (bool, error) {
		// Do not retry if the check's context was cancelled.
		if ctx.Err() != nil {
			return false, ctx.Err()
		}

		newIssue := generateHealthCheckIssueEntry(check, resp, respErr)

		// If no issue is found during the check, we can stop retrying.
		if newIssue == nil {
			return false, nil
		}
		logFailedAttempt(check.RequestMethod, check.RequestURL, trace, respErr)
		issue = newIssue
		return true, nil
	}

	_, err = client.Do(req)
	if issue == nil && err != nil {
		issue = &issueEntry{
			danger,
			fmt.Sprint("Health check failed unexpectedly: ", err),
		}
	}
	if issue != nil && err == nil {
		issue.issueType = warning
		issue.message = fmt.Sprint("Unstable health check: ", issue.message)
	}
	return issue
}

func generateHealthCheckIssueEntry(check HealthCheck, resp *http.Response, respErr error) *issueEntry {
	if respErr != nil {
		return &issueEntry{danger, fmt.Sprintf("%s: cannot be reached", check.RequestURL)}
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return &issueEntry{danger, fmt.Sprintf("%s: response body could not be read", check.RequestURL)}
	}

	if resp.StatusCode != check.ResponseStatusCodeEquals {
		return &issueEntry{danger, fmt.Sprintf("%s: received unexpected status code %d (expected %d)", check.RequestURL, resp.StatusCode, check.ResponseStatusCodeEquals)}
	}

	for key, value := range check.ResponseHeaderContains {
		if resp.Header.Get(key) != value {
			return &issueEntry{danger, fmt.Sprintf("%s: expected response header \"%s: %s\" could not be found", check.RequestURL, key, value)}
		}
	}

	if !strings.Contains(string(respBody), check.ResponseBodyContains) {
		log.Printf("response body %q should contain %q, but it was not found", string(respBody), check.ResponseBodyContains)
		return &issueEntry{danger, fmt.Sprintf("%s: expected response body \"%s\" could not be found", check.RequestURL, check.ResponseBodyContains)}
	}
	return nil
}
