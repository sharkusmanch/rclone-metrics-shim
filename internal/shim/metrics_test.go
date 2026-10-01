package shim

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestFilterRclone_Golden(t *testing.T) {
	raw, err := os.ReadFile("testdata/rclone-1.75.1.metrics")
	if err != nil {
		t.Fatal(err)
	}
	out := string(FilterRclone(raw))
	samples := 0
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if !strings.HasPrefix(line, "rclone_") && !strings.HasPrefix(line, "# HELP rclone_") && !strings.HasPrefix(line, "# TYPE rclone_") {
			t.Fatalf("unexpected line kept: %q", line)
		}
		if !strings.HasPrefix(line, "#") {
			samples++
		}
	}
	if samples != 11 {
		t.Fatalf("want the 11 rclone_ samples of v1.75.1, got %d", samples)
	}
	for _, want := range []string{"rclone_bytes_transferred_total ", "# TYPE rclone_files_deleted_total counter", "rclone_fatal_error "} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatal("output must end with a newline")
	}
}

func TestFilterRclone_Edges(t *testing.T) {
	in := "go_x 1\nrclone_x{a=\"b c\"} 2\n# TYPE rclone_x gauge\n# a comment mentioning rclone_x\nprocess_rclone_y 3\nrclone_last 4"
	got := string(FilterRclone([]byte(in)))
	want := "rclone_x{a=\"b c\"} 2\n# TYPE rclone_x gauge\nrclone_last 4\n"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if out := FilterRclone(nil); len(out) != 0 {
		t.Fatalf("empty input gave %q", out)
	}
	if got := string(FilterRclone([]byte("rclone_a 1\r\ngo_b 2\r\n"))); got != "rclone_a 1\n" {
		t.Fatalf("CRLF: got %q", got)
	}
}

func TestRenderShim_Success(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	out := string(RenderShim(Result{Command: "sync", ExitCode: 0, Start: start, End: start.Add(90500 * time.Millisecond), Scraped: true, LastScrape: start.Add(90 * time.Second)}))
	for _, want := range []string{
		"rclone_shim_exit_code 0\n",
		"rclone_shim_duration_seconds 90.500\n",
		"rclone_shim_last_run_timestamp_seconds 1700000090.500\n",
		"rclone_shim_last_success_timestamp_seconds 1700000090.500\n",
		"rclone_shim_scrape_success 1\n",
		"rclone_shim_last_scrape_age_seconds 0.500\n",
		`rclone_shim_info{version="dev",command="sync"} 1` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	assertHelpAndType(t, out)
}

func TestRenderShim_FailureAndNoScrape(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	out := string(RenderShim(Result{Command: "copy", ExitCode: 7, Start: start, End: start.Add(time.Second)}))
	if !strings.Contains(out, "rclone_shim_exit_code 7\n") || !strings.Contains(out, "rclone_shim_scrape_success 0\n") {
		t.Fatalf("got:\n%s", out)
	}
	for _, absent := range []string{"last_success", "last_scrape_age"} {
		if strings.Contains(out, absent) {
			t.Fatalf("%s must be omitted:\n%s", absent, out)
		}
	}
	assertHelpAndType(t, out)
}

func TestRenderShim_EscapesLabels(t *testing.T) {
	old := Version
	Version = "v1\"\\\n"
	defer func() { Version = old }()
	out := string(RenderShim(Result{Command: "sync"}))
	if !strings.Contains(out, `version="v1\"\\\n"`) {
		t.Fatalf("label not escaped:\n%s", out)
	}
}

func assertHelpAndType(t *testing.T, out string) {
	t.Helper()
	if !strings.HasSuffix(out, "\n") {
		t.Fatal("must end with newline")
	}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		name := metricName(line)
		if !strings.Contains(out, "# HELP "+name+" ") || !strings.Contains(out, "# TYPE "+name+" gauge\n") {
			t.Fatalf("%s lacks HELP/TYPE", name)
		}
	}
}
