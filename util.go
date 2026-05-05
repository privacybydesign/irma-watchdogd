package main

import (
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

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
