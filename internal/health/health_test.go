package health

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheckHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	defer srv.Close()

	// Health check should pass
	ok := Check("127.0.0.1", portFromURL(srv.URL), "/", 2*time.Second)
	if !ok {
		t.Error("expected healthy, got unhealthy")
	}
}

func TestCheckUnhealthyStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ok := Check("127.0.0.1", portFromURL(srv.URL), "/", 2*time.Second)
	if ok {
		t.Error("expected unhealthy (500), got healthy")
	}
}

func TestCheckConnectionRefused(t *testing.T) {
	// Closed port should fail
	ok := Check("127.0.0.1", 19999, "/", 500*time.Millisecond)
	if ok {
		t.Error("expected unhealthy (connection refused), got healthy")
	}
}

func TestCheckCustomPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	ok := Check("127.0.0.1", portFromURL(srv.URL), "/healthz", 2*time.Second)
	if !ok {
		t.Error("expected healthy on /healthz, got unhealthy")
	}

	ok = Check("127.0.0.1", portFromURL(srv.URL), "/badpath", 2*time.Second)
	if ok {
		t.Error("expected unhealthy on /badpath, got healthy")
	}
}

func TestCheckZeroZeroZeroZeroConversion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// "0.0.0.0" should be converted to "127.0.0.1"
	ok := Check("0.0.0.0", portFromURL(srv.URL), "/", 2*time.Second)
	if !ok {
		t.Error("expected healthy with 0.0.0.0→127.0.0.1 conversion")
	}
}

func portFromURL(url string) int {
	var port int
	// Parse "http://127.0.0.1:PORT"
	for i := len(url) - 1; i >= 0; i-- {
		if url[i] == ':' {
			for j := i + 1; j < len(url); j++ {
				port = port*10 + int(url[j]-'0')
			}
			break
		}
	}
	return port
}

func TestCheckRejectsRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	// Before the fix the default client followed the 302 and reported the
	// unrelated target as a healthy Quack endpoint.
	if Check("127.0.0.1", portFromURL(srv.URL), "/", 2*time.Second) {
		t.Error("a 302 redirect must not count as a healthy Quack endpoint")
	}
}

func TestCheckHealthPathCannotChangeHost(t *testing.T) {
	var gotHost, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath = r.Host, r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Under string concatenation this payload turns the URL into
	// http://127.0.0.1:PORThttp://127.0.0.1:9491@evil.com/ and the request
	// is sent to evil.com. A structurally built URL pins the host.
	payload := "/@evil.com/"
	if !Check("127.0.0.1", portFromURL(srv.URL), payload, 2*time.Second) {
		t.Fatal("expected the local test server to be reached")
	}
	if gotHost == "evil.com" || gotPath != payload {
		t.Errorf("host/path was influenced by health_path: host=%q path=%q", gotHost, gotPath)
	}
}

func TestCheckWithExpectRejectsWrongBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("some other http service"))
	}))
	defer srv.Close()

	p := portFromURL(srv.URL)
	if !Check("127.0.0.1", p, "/", 2*time.Second) {
		t.Fatal("liveness-only probe should accept any 2xx")
	}
	if CheckWithExpect("127.0.0.1", p, "/", 2*time.Second, "quack-greeting") {
		t.Error("an unrelated 2xx service must not satisfy an identity-pinned probe (R-09)")
	}

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("quack-greeting v1"))
	}))
	defer ok.Close()
	if !CheckWithExpect("127.0.0.1", portFromURL(ok.URL), "/", 2*time.Second, "quack-greeting") {
		t.Error("matching body must be accepted")
	}
}
