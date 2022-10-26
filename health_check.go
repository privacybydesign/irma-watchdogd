package main

import (
	"context"
	"fmt"
	"github.com/hashicorp/go-retryablehttp"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
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
	var waitGroup sync.WaitGroup
	waitGroup.Add(len(checks))
	issueChan := make(chan *issueEntry, len(checks))

	for _, check := range checks {
		check := check
		go func() {
			issueChan <- runHealthCheck(check)
			waitGroup.Done()
		}()
		// Introduce a small delay to prevent all checks to be started at the same time.
		time.Sleep(10 * time.Millisecond)
	}

	waitGroup.Wait()
	close(issueChan)

	for issue := range issueChan {
		if issue != nil {
			issues = append(issues, *issue)
		}
	}
	return
}

func runHealthCheck(check HealthCheck) *issueEntry {
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

	var issue *issueEntry

	client := newHTTPClient()
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
		return &issueEntry{danger, fmt.Sprintf("%s: received unexpected status code %d", check.RequestURL, resp.StatusCode)}
	}

	for key, value := range check.ResponseHeaderContains {
		if resp.Header.Get(key) != value {
			return &issueEntry{danger, fmt.Sprintf("%s: expected response header \"%s: %s\" could not be found", check.RequestURL, key, value)}
		}
	}

	if !strings.Contains(string(respBody), check.ResponseBodyContains) {
		return &issueEntry{danger, fmt.Sprintf("%s: expected response body \"%s\" could not be found", check.RequestURL, check.ResponseBodyContains)}
	}
	return nil
}
