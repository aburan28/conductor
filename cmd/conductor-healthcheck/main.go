// Command conductor-healthcheck probes the local daemon without a shell or external
// runtime dependencies. It reports health exclusively through its exit status.
package main

import (
	"net/http"
	"os"
	"time"
)

const (
	probeURL     = "http://127.0.0.1:8080/v1/live"
	probeTimeout = 4 * time.Second
)

func main() {
	if !healthy(newProbeClient(probeTimeout), probeURL) {
		os.Exit(1)
	}
}

func newProbeClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// A probe must reach the local process directly, regardless of proxy settings.
			Proxy:                  nil,
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: 64 << 10,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func healthy(client *http.Client, endpoint string) bool {
	resp, err := client.Get(endpoint)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Health is the status alone. Never read, print, or wait for a response body.
	return resp.StatusCode == http.StatusOK
}
