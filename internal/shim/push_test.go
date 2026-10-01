package shim

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPushURL(t *testing.T) {
	cases := []struct {
		job, instance, want string
	}{
		{"rclone", "", "http://p/metrics/job/rclone"},
		{"rclone", "books", "http://p/metrics/job/rclone/instance/books"},
		{"rclone", "a/b", "http://p/metrics/job/rclone/instance@base64/YS9i"},
		{"my job", "x", "http://p/metrics/job/my%20job/instance/x"},
	}
	for _, c := range cases {
		if got := PushURL(Config{PushgatewayURL: "http://p", Job: c.job, Instance: c.instance}); got != c.want {
			t.Errorf("job=%q instance=%q: got %s want %s", c.job, c.instance, got, c.want)
		}
	}
}

func TestPush_PostsBody(t *testing.T) {
	var method, path, ctype, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, path, ctype, body = r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(b)
	}))
	defer srv.Close()
	err := Push(context.Background(), Config{PushgatewayURL: srv.URL, Job: "rclone", Instance: "t"}, []byte("a 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if method != "POST" || path != "/metrics/job/rclone/instance/t" || !strings.HasPrefix(ctype, "text/plain") || body != "a 1\n" {
		t.Fatalf("method=%s path=%s ctype=%s body=%q", method, path, ctype, body)
	}
}

func TestPush_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "pushed metrics are invalid\nmore", http.StatusBadRequest)
	}))
	defer srv.Close()
	err := Push(context.Background(), Config{PushgatewayURL: srv.URL, Job: "rclone"}, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "pushed metrics are invalid") {
		t.Fatalf("got %v", err)
	}
}

func TestPush_HonoursDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Push(ctx, Config{PushgatewayURL: srv.URL, Job: "rclone"}, []byte("x"))
	if err == nil {
		t.Fatal("want an error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("push took %s, deadline not honoured", d)
	}
}
