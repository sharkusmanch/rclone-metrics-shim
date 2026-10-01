package shim

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// shortTempDir avoids the sockaddr_un length limit that t.TempDir can exceed on macOS.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "shim")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func serveUnix(t *testing.T, socket string, h http.Handler) {
	t.Helper()
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestScraper_ReturnsLatestBody(t *testing.T) {
	socket := filepath.Join(shortTempDir(t), "m.sock")
	var n atomic.Int64
	serveUnix(t, socket, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "rclone_n %d\n", n.Add(1))
	}))
	s := StartScraper(socket, 100*time.Millisecond)
	waitFor(t, "three scrapes", func() bool { return n.Load() >= 3 })
	body, at, ok := s.Stop()
	if !ok || at.IsZero() {
		t.Fatalf("ok=%v at=%v", ok, at)
	}
	if got, last := string(body), fmt.Sprintf("rclone_n %d\n", n.Load()); got != last {
		t.Fatalf("got %q, want the latest %q", got, last)
	}
}

func TestScraper_ServerAppearsLate(t *testing.T) {
	socket := filepath.Join(shortTempDir(t), "m.sock")
	s := StartScraper(socket, time.Hour) // only the fast initial retry can succeed
	time.Sleep(300 * time.Millisecond)
	serveUnix(t, socket, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "rclone_a 1\n") }))
	waitFor(t, "first scrape", s.have)
	if body, _, ok := s.Stop(); !ok || string(body) != "rclone_a 1\n" {
		t.Fatalf("ok=%v body=%q", ok, body)
	}
}

func TestScraper_NeverAppears(t *testing.T) {
	s := StartScraper(filepath.Join(shortTempDir(t), "absent.sock"), 100*time.Millisecond)
	time.Sleep(250 * time.Millisecond)
	start := time.Now()
	_, _, ok := s.Stop()
	if ok {
		t.Fatal("want ok=false")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Stop took %s", d)
	}
	if _, _, ok := s.Stop(); ok { // second Stop is safe
		t.Fatal("want ok=false on second Stop")
	}
}

func TestScraper_IgnoresErrorResponses(t *testing.T) {
	socket := filepath.Join(shortTempDir(t), "m.sock")
	var hits atomic.Int64
	serveUnix(t, socket, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	s := StartScraper(socket, 100*time.Millisecond)
	waitFor(t, "two attempts", func() bool { return hits.Load() >= 2 })
	if _, _, ok := s.Stop(); ok {
		t.Fatal("a 500 must not be stored")
	}
}
