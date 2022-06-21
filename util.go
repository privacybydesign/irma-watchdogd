package main

import (
	"github.com/hashicorp/go-retryablehttp"
	"time"
)

func newHTTPClient() *retryablehttp.Client {
	// Use retryablehttp to prevent false positives.
	client := retryablehttp.NewClient()
	client.HTTPClient.Timeout = 3 * time.Second
	return client
}
