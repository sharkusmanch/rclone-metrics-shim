//go:build e2e

package shim

import (
	"bytes"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// These tests drive a real rclone binary, named by RCLONE_BIN, through the
// built shim. They pin the behaviour the design depends on: rclone enables its
// metrics endpoint from RCLONE_METRICS_ADDR alone and serves it on a unix socket.

func realRclone(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("RCLONE_BIN")
	if bin == "" {
		t.Skip("RCLONE_BIN not set")
	}
	return bin
}

// e2eDirs installs the shim as "rclone" ahead of the real binary and returns
// the PATH to use plus a work directory.
func e2eDirs(t *testing.T) (pathEnv, work string) {
	t.Helper()
	real := realRclone(t)
	work = shortTempDir(t)
	shimDir, realDir := filepath.Join(work, "shim"), filepath.Join(work, "real")
	if err := Install(shimBinary(t), filepath.Join(shimDir, "rclone")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(realDir, "rclone")); err != nil {
		t.Fatal(err)
	}
	return shimDir + string(os.PathListSeparator) + realDir, work
}

func TestE2E_RealRcloneSync(t *testing.T) {
	pathEnv, work := e2eDirs(t)
	src, dst := filepath.Join(work, "src"), filepath.Join(work, "dst")
	for _, d := range []string{src, dst} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	big := make([]byte, 20<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(src, "f"+strconv.Itoa(i)+".txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dst, "stale.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	pgw := newPushRecorder(t)
	cmd := exec.Command(filepath.Join(work, "shim", "rclone"), "sync", src, dst, "--bwlimit", "10M")
	// RCLONE_LOCAL_NO_CLONE: on macOS a local copy is an instant APFS clone that
	// ignores --bwlimit, leaving nothing to scrape. Set by env because the flag
	// does not exist on every platform; an unknown RCLONE_* variable is ignored.
	cmd.Env = []string{"PATH=" + pathEnv, "HOME=" + work, "RCLONE_CONFIG=" + filepath.Join(work, "rclone.conf"), "RCLONE_LOCAL_NO_CLONE=true",
		"RCLONESHIM_PUSHGATEWAY_URL=" + pgw.URL, "RCLONESHIM_INSTANCE=e2e", "RCLONESHIM_INTERVAL=200ms"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("rclone sync through the shim: %v\n%s", err, stderr.String())
	}
	if lines := shimLines(stderr.String()); len(lines) != 0 {
		t.Fatalf("shim was not silent: %q", lines)
	}

	got, err := os.ReadFile(filepath.Join(dst, "big.bin"))
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("destination differs from source (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale.txt")); !os.IsNotExist(err) {
		t.Fatal("sync must have deleted the stale file")
	}

	body := pgw.final(t).Body
	for _, want := range []string{"rclone_shim_exit_code 0\n", "rclone_shim_scrape_success 1\n", "# TYPE rclone_bytes_transferred_total counter\n"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	m := regexp.MustCompile(`(?m)^rclone_bytes_transferred_total (\S+)$`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no rclone_bytes_transferred_total in:\n%s", body)
	}
	if v, err := strconv.ParseFloat(m[1], 64); err != nil || v <= 0 {
		t.Fatalf("rclone_bytes_transferred_total = %q, want > 0", m[1])
	}
	if strings.Contains(body, "\ngo_") || strings.Contains(body, "\nprocess_") {
		t.Fatal("runtime series leaked into the push")
	}
}

func TestE2E_RealRcloneLsfIsPassthrough(t *testing.T) {
	pathEnv, work := e2eDirs(t)
	src := filepath.Join(work, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "only.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pgw := newPushRecorder(t)
	cmd := exec.Command(filepath.Join(work, "shim", "rclone"), "lsf", src)
	cmd.Env = []string{"PATH=" + pathEnv, "HOME=" + work, "RCLONE_CONFIG=" + filepath.Join(work, "rclone.conf"),
		"RCLONESHIM_PUSHGATEWAY_URL=" + pgw.URL}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "only.txt" {
		t.Fatalf("lsf output %q", out)
	}
	if n := len(pgw.all()); n != 0 {
		t.Fatalf("lsf must not push, got %d requests", n)
	}
}

// A real rclone exit status must survive the shim: syncing a missing source
// fails with rclone's own code, and that is what the caller gets and what is pushed.
func TestE2E_RealRcloneFailureStatus(t *testing.T) {
	pathEnv, work := e2eDirs(t)
	pgw := newPushRecorder(t)
	env := []string{"PATH=" + pathEnv, "HOME=" + work, "RCLONE_CONFIG=" + filepath.Join(work, "rclone.conf")}

	direct := exec.Command(realRclone(t), "sync", filepath.Join(work, "missing"), filepath.Join(work, "dst"))
	direct.Env = env
	_ = direct.Run()
	want := direct.ProcessState.ExitCode()
	if want == 0 {
		t.Fatal("expected the real rclone to fail on a missing source")
	}

	cmd := exec.Command(filepath.Join(work, "shim", "rclone"), "sync", filepath.Join(work, "missing"), filepath.Join(work, "dst"))
	cmd.Env = append(env, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL)
	_ = cmd.Run()
	if got := cmd.ProcessState.ExitCode(); got != want {
		t.Fatalf("exit %d through the shim, %d directly", got, want)
	}
	if !strings.Contains(pgw.final(t).Body, "rclone_shim_exit_code "+strconv.Itoa(want)+"\n") {
		t.Fatalf("pushed exit code is not %d:\n%s", want, pgw.final(t).Body)
	}
	if strings.Contains(pgw.final(t).Body, "last_success") {
		t.Fatal("a failed run must not push last_success")
	}
}
