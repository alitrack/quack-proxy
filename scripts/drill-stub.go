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
// wire protocol: this drill validates the systemd / process-lifecycle contract,
// not the database engine. The real engine is covered by scripts/quack-e2e.sh.
//
// The daemon also runs the same binary as a CLIENT for its auth probe:
//
//	'<duckdb_bin>' -unsigned -c "LOAD quack; FROM quack_query('quack:host:port', 'SELECT <n> AS auth_probe', token := '<tok>', disable_ssl := true);"
//
// A real engine answers that with <n>, proving the shard accepts its own
// token. The stub models the same contract without the wire protocol: the
// server side (boot SQL carries token = '<tok>') exposes POST /quack-auth,
// which echoes the request body only for `Authorization: Bearer <tok>`; the
// client side parses host, marker and token from the probe SQL, posts the
// marker, and prints it back only on a 200. A wrong or missing token, or a
// dead shard, fails the probe exactly as it would on a real engine.
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
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultGreeting = "systemd-drill-greeting"

var serveRE = regexp.MustCompile(`quack_serve\('quack:[^:']+:(\d+)'`)
var serveTokenRE = regexp.MustCompile(`token\s*=\s*'([^']+)'`)

// The auth probe the daemon sends. The marker is random per probe so a stale
// or echoed answer cannot pass; the client must return that exact number.
var queryRE = regexp.MustCompile(`quack_query\('quack:([^':]+):(\d+)'`)
var markerRE = regexp.MustCompile(`SELECT\s+(\d+)\s+AS\s+auth_probe`)
var queryTokenRE = regexp.MustCompile(`token\s*:=?\s*'([^']+)'`)

func main() {
	if sql, ok := clientSQL(os.Args[1:]); ok {
		os.Exit(runClient(sql))
	}
	os.Exit(runServer())
}

// clientSQL reports the SQL of a `-c <sql>` invocation, i.e. the auth probe.
func clientSQL(args []string) (string, bool) {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-c" {
			return args[i+1], true
		}
	}
	return "", false
}

// runClient answers the auth probe: connect to the shard named in the SQL,
// present the token, and print the marker only if the shard accepted it.
func runClient(sql string) int {
	m := queryRE.FindStringSubmatch(sql)
	if m == nil {
		fmt.Fprintln(os.Stderr, "drill-stub: -c given but no quack_query(...) target found")
		return 1
	}
	host, port := m[1], m[2]
	mk := markerRE.FindStringSubmatch(sql)
	if mk == nil {
		fmt.Fprintln(os.Stderr, "drill-stub: -c given but no 'SELECT <n> AS auth_probe' marker found")
		return 1
	}
	marker := mk[1]
	tok := ""
	if t := queryTokenRE.FindStringSubmatch(sql); t != nil {
		tok = t[1]
	}
	if tok == "" {
		fmt.Fprintln(os.Stderr, "drill-stub: auth probe carried no token")
		return 1
	}
	addr := net.JoinHostPort(host, port)
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/quack-auth", strings.NewReader(marker))
	if err != nil {
		fmt.Fprintln(os.Stderr, "drill-stub:", err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "drill-stub: auth probe could not reach", addr+":", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "drill-stub: shard refused the token (HTTP %d)\n", resp.StatusCode)
		return 1
	}
	if strings.TrimSpace(string(body)) != marker {
		fmt.Fprintf(os.Stderr, "drill-stub: shard answered %q, want marker %s\n", strings.TrimSpace(string(body)), marker)
		return 1
	}
	fmt.Println(marker)
	return 0
}

// runServer is the shard side: parse the boot SQL, serve the greeting on the
// quack_serve port, and (when the boot SQL carries a token) authenticate the
// probe on /quack-auth.
func runServer() int {
	portCh := make(chan string, 1)
	tokenCh := make(chan string, 1)
	go func() {
		var token string
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := scanner.Text()
			if token == "" {
				if t := serveTokenRE.FindStringSubmatch(line); t != nil {
					token = t[1]
				}
			}
			if m := serveRE.FindStringSubmatch(line); m != nil {
				portCh <- m[1]
				tokenCh <- token
				// Keep draining: the `tail -f /dev/null` writer never closes
				// stdin, and we must not make it block on a full pipe.
				for scanner.Scan() {
				}
				return
			}
		}
		portCh <- ""
		tokenCh <- ""
	}()

	var port, token string
	select {
	case port = <-portCh:
		token = <-tokenCh
	case <-time.After(10 * time.Second):
		fmt.Fprintln(os.Stderr, "drill-stub: no quack_serve line seen in boot SQL")
		return 1
	}
	if port == "" {
		fmt.Fprintln(os.Stderr, "drill-stub: boot SQL ended before quack_serve")
		return 1
	}

	greeting := os.Getenv("DRILL_STUB_GREETING")
	if greeting == "" {
		greeting = defaultGreeting
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, greeting)
	})
	if token != "" {
		mux.HandleFunc("/quack-auth", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "Authentication failed", http.StatusForbidden)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, 64))
			if err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if _, err := strconv.Atoi(strings.TrimSpace(string(body))); err != nil {
				http.Error(w, "bad marker", http.StatusBadRequest)
				return
			}
			_, _ = w.Write(bytes.TrimSpace(body))
		})
	}
	bind := os.Getenv("DRILL_STUB_BIND")
	if bind == "" {
		bind = "127.0.0.1"
	}
	srv := &http.Server{Addr: net.JoinHostPort(bind, port), Handler: mux}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "drill-stub:", err)
		return 1
	}
	return 0
}
