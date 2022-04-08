package main

import (
	"fmt"
	"github.com/hashicorp/go-retryablehttp"
	"io/ioutil"
	"log"
	"strings"
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
	for _, check := range checks {
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
			issues = append(issues, issueEntry{warning, fmt.Sprintf("%s: invalid health check", check.RequestURL)})
			continue
		}
		for key, value := range check.RequestHeaders {
			req.Header.Set(key, value)
		}

		resp, err := retryablehttp.NewClient().Do(req)
		if err != nil {
			issues = append(issues, issueEntry{danger, fmt.Sprintf("%s: cannot not be reached", check.RequestURL)})
			continue
		}
		if resp.StatusCode != check.ResponseStatusCodeEquals {
			issues = append(issues, issueEntry{danger, fmt.Sprintf("%s: received unexpected status code %d", check.RequestURL, resp.StatusCode)})
			continue
		}

		for key, value := range check.ResponseHeaderContains {
			if resp.Header.Get(key) != value {
				issues = append(issues, issueEntry{danger, fmt.Sprintf("%s: expected response header \"%s: %s\" could not be found", check.RequestURL, key, value)})
			}
		}

		respBody, err := ioutil.ReadAll(resp.Body)
		if !strings.Contains(string(respBody), check.ResponseBodyContains) {
			issues = append(issues, issueEntry{danger, fmt.Sprintf("%s: expected response body \"%s\" could not be found", check.RequestURL, check.ResponseBodyContains)})
		}
	}
	return
}
