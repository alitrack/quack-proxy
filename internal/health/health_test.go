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
