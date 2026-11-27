// Package health provides HTTP health check for Quack endpoints.
package health

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// The probe runs every few seconds for every shard, so reuse connections
// instead of constructing a fresh http.Client (and a fresh idle pool that is
// never closed) on each tick.
var transport = &http.Transport{
	MaxIdleConns:          64,
	MaxIdleConnsPerHost:   4,
	IdleConnTimeout:       30 * time.Second,
	ResponseHeaderTimeout: 5 * time.Second,
}

var client = &http.Client{
	Transport: transport,
	// A 3xx is not a healthy Quack endpoint. Following redirects would let
	// any HTTP server that happens to hold the port satisfy the probe (and
	// would also let a crafted path steer the probe at another host).
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Check performs a single HTTP GET health check against a Quack endpoint.
// It returns true only when the endpoint answers with a 2xx within timeout.
//
// NOTE: this is a liveness probe, not a protocol check. A 2xx from an
// unrelated HTTP server on the same port still produces a false positive;
// a real Quack handshake check is tracked as R-09 in docs/REMEDIATION.md.
func Check(host string, port int, path string, timeout time.Duration) bool {
	return CheckWithExpect(host, port, path, timeout, "")
}

// CheckWithExpect behaves like Check but additionally requires the response
// body to contain expect. This turns the liveness probe into a weak identity
// check: an unrelated HTTP server that happens to hold the shard's port will
// normally not emit the Quack greeting (R-09). It is NOT a protocol handshake
// or an authentication check — see docs/SECURITY.md.
func CheckWithExpect(host string, port int, path string, timeout time.Duration, expect string) bool {
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if path == "" {
		path = "/"
	}
	// Build the URL structurally. The old fmt.Sprintf("http://%s:%d%s", ...)
	// let a crafted health_path inject userinfo ("...@evil.com") and turn the
	// probe into an SSRF; url.URL escapes the path and pins the host (R-09).
	u := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, fmt.Sprintf("%d", port)),
		Path:   path,
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the connection can be reused, and so a
	// half-finished response cannot look like a completed healthy probe.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	if expect != "" && !bytes.Contains(body, []byte(expect)) {
		return false
	}
	return true
}
