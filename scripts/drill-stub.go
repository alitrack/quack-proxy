//go:build ignore

// drill-stub is a minimal stand-in for the DuckDB CLI, used only by the
// systemd install drill (scripts/systemd-install-drill.sh).
//
// quack-proxy feeds each shard its boot SQL through a pipeline:
//
//	(cat boot.sql; tail -f /dev/null) | '<duckdb_bin>' '<database>'
//
// This stub reads that stream, extracts the port from the quack_serve(...)
// line, and serves that port with a fixed greeting so the daemon's health
// loop can reach "healthy". It deliberately does NOT implement the Quack
// protocol: this drill validates the systemd / process-lifecycle contract,
// not the database engine. The real engine is covered by scripts/quack-e2e.sh.
//
// By default it listens on 127.0.0.1. Set DRILL_STUB_BIND=0.0.0.0 to expose
// the stand-in endpoint on a LAN interface; scripts/verify-plaintext.sh uses
// that to prove (with a real packet capture) that quack-proxy traffic carries
// the shard token in cleartext. The default is unchanged for the systemd
// drill.
//
// DRILL_STUB_GREETING overrides the fixed response body. The VIP failover
// drill (scripts/verify-vip-failover.sh) runs one stub per node with distinct
// greetings so the client can prove WHICH node answered through the floating
// IP. Default is unchanged for every existing caller.
//
// Build (the `ignore` tag keeps it out of `go build ./...` and coverage):
//
//	go build -o /tmp/drill-stub ./scripts/drill-stub.go
package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"time"
)

const defaultGreeting = "systemd-drill-greeting"

var serveRE = regexp.MustCompile(`quack_serve\('quack:[^:']+:(\d+)'`)

func main() {
	portCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			if m := serveRE.FindStringSubmatch(scanner.Text()); m != nil {
				portCh <- m[1]
				// Keep draining: the `tail -f /dev/null` writer never closes
				// stdin, and we must not make it block on a full pipe.
				for scanner.Scan() {
				}
				return
			}
		}
		portCh <- ""
	}()

	var port string
	select {
	case port = <-portCh:
	case <-time.After(10 * time.Second):
		fmt.Fprintln(os.Stderr, "drill-stub: no quack_serve line seen in boot SQL")
		os.Exit(1)
	}
	if port == "" {
		fmt.Fprintln(os.Stderr, "drill-stub: boot SQL ended before quack_serve")
		os.Exit(1)
	}

	greeting := os.Getenv("DRILL_STUB_GREETING")
	if greeting == "" {
		greeting = defaultGreeting
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, greeting)
	})
	bind := os.Getenv("DRILL_STUB_BIND")
	if bind == "" {
		bind = "127.0.0.1"
	}
	srv := &http.Server{Addr: net.JoinHostPort(bind, port), Handler: mux}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "drill-stub:", err)
		os.Exit(1)
	}
}
