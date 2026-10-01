package shim

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The fake rclone is this test binary re-executed with SHIM_FAKE_RCLONE=1. It
// is installed in a PATH directory under the name "rclone".

type fakeRecord struct {
	Args        []string
	MetricsAddr string
	HasAddr     bool
	Pid         int
	Stdin       string
	StdoutInode uint64
}

func TestMain(m *testing.M) {
	if os.Getenv("SHIM_FAKE_RCLONE") == "1" {
		fakeRclone()
	}
	code := m.Run()
	if builtShim != "" {
		os.RemoveAll(filepath.Dir(builtShim))
	}
	os.Exit(code)
}

func fakeRclone() {
	rec := fakeRecord{Args: os.Args, Pid: os.Getpid()}
	rec.MetricsAddr, rec.HasAddr = os.LookupEnv("RCLONE_METRICS_ADDR")
	if os.Getenv("FAKE_READ_STDIN") == "1" {
		b, _ := io.ReadAll(os.Stdin)
		rec.Stdin = string(b)
	}
	if info, err := os.Stdout.Stat(); err == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			rec.StdoutInode = uint64(st.Ino)
		}
	}
	if path := os.Getenv("FAKE_RECORD"); path != "" {
		b, _ := json.Marshal(rec)
		_ = os.WriteFile(path, b, 0o644)
	}
	if body := os.Getenv("FAKE_METRICS"); body != "" && strings.HasPrefix(rec.MetricsAddr, "unix://") {
		l, err := net.Listen("unix", strings.TrimPrefix(rec.MetricsAddr, "unix://"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake: listen:", err)
			os.Exit(98)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) })
		go http.Serve(l, mux)
	}
	if s := os.Getenv("FAKE_STDOUT"); s != "" {
		fmt.Fprint(os.Stdout, s)
	}
	if s := os.Getenv("FAKE_STDERR"); s != "" {
		fmt.Fprint(os.Stderr, s)
	}

	if os.Getenv("FAKE_WAIT_SIGNAL") == "1" {
		c := make(chan os.Signal, 8)
		signal.Notify(c, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGQUIT)
		if path := os.Getenv("FAKE_READY"); path != "" {
			_ = os.WriteFile(path, []byte("ready"), 0o644)
		}
		for {
			select {
			case s := <-c:
				f, _ := os.OpenFile(os.Getenv("FAKE_SIGNALS"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				fmt.Fprintln(f, s.String())
				f.Close()
				if s == syscall.SIGTERM { // die by the signal, as rclone does
					signal.Reset(syscall.SIGTERM)
					_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
					time.Sleep(5 * time.Second)
				}
			case <-time.After(20 * time.Second):
				os.Exit(99)
			}
		}
	}
	if path := os.Getenv("FAKE_READY"); path != "" {
		_ = os.WriteFile(path, []byte("ready"), 0o644)
	}
	if d, err := time.ParseDuration(os.Getenv("FAKE_SLEEP")); err == nil {
		time.Sleep(d)
	}
	code, _ := strconv.Atoi(os.Getenv("FAKE_EXIT"))
	os.Exit(code)
}

// pushRecorder is a fake Pushgateway.
type pushRecorder struct {
	*httptest.Server
	mu       sync.Mutex
	requests []pushRequest
	hang     func(body string) bool // block this request until the test ends
	release  chan struct{}
}

type pushRequest struct{ Method, Path, Body string }

func newPushRecorder(t *testing.T) *pushRecorder {
	t.Helper()
	p := &pushRecorder{release: make(chan struct{})}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.requests = append(p.requests, pushRequest{r.Method, r.URL.Path, string(b)})
		hang := p.hang != nil && p.hang(string(b))
		p.mu.Unlock()
		if hang {
			select {
			case <-p.release:
			case <-r.Context().Done():
			}
		}
	}))
	t.Cleanup(func() { close(p.release); p.Server.Close() })
	return p
}

func (p *pushRecorder) all() []pushRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]pushRequest(nil), p.requests...)
}

// final returns the push that carries the exit code.
func (p *pushRecorder) final(t *testing.T) pushRequest {
	t.Helper()
	for _, r := range p.all() {
		if strings.Contains(r.Body, "rclone_shim_exit_code") {
			return r
		}
	}
	t.Fatalf("no final push among %d requests: %+v", len(p.all()), p.all())
	return pushRequest{}
}

// harness runs Run in-process against the fake rclone.
type harness struct {
	t                    *testing.T
	dir, record          string
	env                  []string
	outFile, errFile, in *os.File
}

func newHarness(t *testing.T, extraEnv ...string) *harness {
	t.Helper()
	dir := shortTempDir(t)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(bin, "rclone")); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, dir: dir, record: filepath.Join(dir, "record.json")}
	open := func(name string) *os.File {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	h.outFile, h.errFile = open("stdout"), open("stderr")
	h.in, err = os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.in.Close() })
	h.env = append([]string{
		"PATH=" + bin,
		"SHIM_FAKE_RCLONE=1",
		"FAKE_RECORD=" + h.record,
		"RCLONESHIM_INTERVAL=100ms",
	}, extraEnv...)
	return h
}

func (h *harness) run(args ...string) int {
	return Run("/called/as/rclone", args, h.env, "", Stdio{In: h.in, Out: h.outFile, Err: h.errFile})
}

func (h *harness) rec() fakeRecord {
	h.t.Helper()
	b, err := os.ReadFile(h.record)
	if err != nil {
		h.t.Fatalf("fake rclone did not run: %v", err)
	}
	var r fakeRecord
	if err := json.Unmarshal(b, &r); err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *harness) read(f *os.File) string {
	h.t.Helper()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func shimLines(stderr string) []string {
	var out []string
	for _, l := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(l, "rclone-metrics-shim: ") {
			out = append(out, l)
		}
	}
	return out
}

func TestRun_PassesArgsAndExitCode(t *testing.T) {
	pgw := newPushRecorder(t)
	args := []string{"sync", "/src dir", "", "remote:päth/✓", "--exclude", "*.partial"}
	for _, code := range []int{0, 1, 7} {
		h := newHarness(t, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL, "FAKE_EXIT="+strconv.Itoa(code))
		if got := h.run(args...); got != code {
			t.Fatalf("exit %d, want %d", got, code)
		}
		rec := h.rec()
		if want := append([]string{"/called/as/rclone"}, args...); fmt.Sprintf("%q", rec.Args) != fmt.Sprintf("%q", want) {
			t.Fatalf("args %q, want %q", rec.Args, want)
		}
	}
}

func TestRun_StdioInherited(t *testing.T) {
	pgw := newPushRecorder(t)
	h := newHarness(t, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL, "FAKE_STDOUT=to-stdout", "FAKE_STDERR=to-stderr", "FAKE_READ_STDIN=1")
	stdinPath := filepath.Join(h.dir, "stdin")
	if err := os.WriteFile(stdinPath, []byte("from-stdin"), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	h.in = in
	if code := h.run("sync", "a", "b"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	rec := h.rec()
	if rec.Stdin != "from-stdin" {
		t.Fatalf("stdin %q", rec.Stdin)
	}
	// The child's fd 1 is the very file we passed, not a pipe through the shim.
	info, _ := h.outFile.Stat()
	if want := uint64(info.Sys().(*syscall.Stat_t).Ino); rec.StdoutInode != want {
		t.Fatalf("child stdout inode %d, want %d (inherited)", rec.StdoutInode, want)
	}
	// stdout carries the child's bytes and nothing from the shim.
	if got := h.read(h.outFile); got != "to-stdout" {
		t.Fatalf("stdout %q", got)
	}
	if got := h.read(h.errFile); got != "to-stderr" {
		t.Fatalf("stderr %q: the shim must be silent on success", got)
	}
}

func TestRun_PushesScrapeAndShimSeries(t *testing.T) {
	pgw := newPushRecorder(t)
	h := newHarness(t, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL, "RCLONESHIM_INSTANCE=t",
		"FAKE_METRICS=# TYPE rclone_files_transferred_total counter\nrclone_files_transferred_total 5\ngo_goroutines 3\n", "FAKE_SLEEP=600ms")
	if code := h.run("sync", "a", "b"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	reqs := pgw.all()
	if len(reqs) != 2 {
		t.Fatalf("want a start push and a final push, got %d: %+v", len(reqs), reqs)
	}
	if !strings.Contains(reqs[0].Body, "rclone_shim_last_start_timestamp_seconds") || strings.Contains(reqs[0].Body, "exit_code") {
		t.Fatalf("first push must be the start announcement:\n%s", reqs[0].Body)
	}
	final := pgw.final(t)
	if final.Method != "POST" || final.Path != "/metrics/job/rclone/instance/t" {
		t.Fatalf("%s %s", final.Method, final.Path)
	}
	for _, want := range []string{
		"rclone_files_transferred_total 5\n", "# TYPE rclone_files_transferred_total counter\n",
		"rclone_shim_exit_code 0\n", "rclone_shim_last_success_timestamp_seconds ", "rclone_shim_scrape_success 1\n",
		`rclone_shim_info{version="dev",command="sync"} 1`,
	} {
		if !strings.Contains(final.Body, want) {
			t.Fatalf("missing %q in:\n%s", want, final.Body)
		}
	}
	if strings.Contains(final.Body, "go_goroutines") {
		t.Fatalf("runtime series leaked:\n%s", final.Body)
	}
}

func TestRun_FailureOmitsLastSuccess(t *testing.T) {
	pgw := newPushRecorder(t)
	h := newHarness(t, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL, "FAKE_EXIT=7")
	if code := h.run("sync", "a", "b"); code != 7 {
		t.Fatalf("exit %d", code)
	}
	body := pgw.final(t).Body
	if !strings.Contains(body, "rclone_shim_exit_code 7\n") || strings.Contains(body, "last_success") {
		t.Fatalf("body:\n%s", body)
	}
}

func TestRun_NoScrape(t *testing.T) {
	pgw := newPushRecorder(t)
	h := newHarness(t, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL, "FAKE_EXIT=1") // never serves metrics
	if code := h.run("sync", "a", "b"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	body := pgw.final(t).Body
	if !strings.Contains(body, "rclone_shim_scrape_success 0\n") || !strings.Contains(body, "rclone_shim_last_scrape_age_seconds NaN\n") {
		t.Fatalf("body:\n%s", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "rclone_") && !strings.HasPrefix(line, "rclone_shim_") {
			t.Fatalf("unexpected rclone series without a scrape: %q", line)
		}
	}
}

func TestRun_PushgatewayDownIsTransparent(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens here now
	h := newHarness(t, "RCLONESHIM_PUSHGATEWAY_URL=http://"+addr, "FAKE_EXIT=3", "FAKE_STDOUT=out")
	if code := h.run("sync", "a", "b"); code != 3 {
		t.Fatalf("exit %d, want the child's 3", code)
	}
	lines := shimLines(h.read(h.errFile))
	if len(lines) != 1 || !strings.Contains(lines[0], "push failed") {
		t.Fatalf("want exactly one warning line, got %q", lines)
	}
	if got := h.read(h.outFile); got != "out" {
		t.Fatalf("stdout %q", got)
	}
}

func TestRun_PushHangsIsBoundedAndTransparent(t *testing.T) {
	pgw := newPushRecorder(t)
	pgw.hang = func(string) bool { return true }
	h := newHarness(t, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL, "RCLONESHIM_PUSH_TIMEOUT=300ms", "FAKE_EXIT=5")
	start := time.Now()
	if code := h.run("sync", "a", "b"); code != 5 {
		t.Fatalf("exit %d, want the child's 5", code)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Run took %s with a hanging pushgateway", d)
	}
}

func TestRun_NoTempDirStillRuns(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(shortTempDir(t), "does-not-exist"))
	pgw := newPushRecorder(t)
	h := newHarness(t, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL, "FAKE_EXIT=4")
	if code := h.run("sync", "a", "b"); code != 4 {
		t.Fatalf("exit %d", code)
	}
	if rec := h.rec(); rec.HasAddr || len(rec.Args) != 4 {
		t.Fatalf("child must run unchanged without a metrics address: %+v", rec)
	}
	if !strings.Contains(pgw.final(t).Body, "rclone_shim_scrape_success 0\n") {
		t.Fatal("scrape_success must be 0")
	}
	if lines := shimLines(h.read(h.errFile)); len(lines) != 1 || !strings.Contains(lines[0], "no temp dir") {
		t.Fatalf("warnings: %q", lines)
	}
}

func TestRun_SocketPathTooLong(t *testing.T) {
	long := filepath.Join(shortTempDir(t), strings.Repeat("x", 120))
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", long)
	pgw := newPushRecorder(t)
	h := newHarness(t, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL)
	if code := h.run("sync", "a", "b"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if h.rec().HasAddr {
		t.Fatal("no metrics address must be set when the socket path cannot fit")
	}
	entries, _ := os.ReadDir(long)
	if len(entries) != 0 {
		t.Fatalf("temp dir left behind: %v", entries)
	}
}

func TestRun_ChildEnvOnlyAndTempDirRemoved(t *testing.T) {
	pgw := newPushRecorder(t)
	h := newHarness(t, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL)
	// Spare capacity: a careless append would write into the caller's array.
	h.env = append(make([]string, 0, len(h.env)+8), h.env...)
	if code := h.run("copy", "a", "b"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	rec := h.rec()
	if !strings.HasPrefix(rec.MetricsAddr, "unix:///") || !strings.HasSuffix(rec.MetricsAddr, "/m.sock") {
		t.Fatalf("child metrics addr %q", rec.MetricsAddr)
	}
	if _, set := os.LookupEnv("RCLONE_METRICS_ADDR"); set {
		t.Fatal("the shim's own environment must not change")
	}
	for _, e := range h.env[:cap(h.env)] {
		if strings.HasPrefix(e, "RCLONE_METRICS_ADDR=") {
			t.Fatal("the caller's env slice must not be modified")
		}
	}
	if _, err := os.Stat(filepath.Dir(strings.TrimPrefix(rec.MetricsAddr, "unix://"))); !os.IsNotExist(err) {
		t.Fatalf("socket directory must be removed, stat err = %v", err)
	}
}

func TestRun_CallerMetricsAddrNotOverridden(t *testing.T) {
	pgw := newPushRecorder(t)
	h := newHarness(t, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL, "RCLONE_METRICS_ADDR=:1")
	if code := h.run("sync", "a", "b"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if rec := h.rec(); rec.MetricsAddr != ":1" {
		t.Fatalf("child saw %q, want the caller's :1", rec.MetricsAddr)
	}
	if !strings.Contains(pgw.final(t).Body, "rclone_shim_exit_code 0\n") {
		t.Fatal("shim series must still be pushed")
	}
}

func TestRun_RealNotFound(t *testing.T) {
	h := newHarness(t)
	h.env[0] = "PATH=" + shortTempDir(t)
	if code := h.run("sync", "a", "b"); code != 127 {
		t.Fatalf("exit %d, want 127", code)
	}
	if lines := shimLines(h.read(h.errFile)); len(lines) != 1 {
		t.Fatalf("stderr: %q", lines)
	}
}

func TestRun_PassthroughExecsUnchanged(t *testing.T) {
	type call struct {
		path string
		argv []string
		env  []string
	}
	var got *call
	old := execve
	execve = func(path string, argv, env []string) error {
		got = &call{path, argv, env}
		return syscall.ENOEXEC
	}
	defer func() { execve = old }()

	pgw := newPushRecorder(t)
	cases := map[string]struct {
		env  []string
		args []string
	}{
		"no pushgateway configured":  {nil, []string{"sync", "a", "b"}},
		"subcommand not wrapped":     {[]string{"RCLONESHIM_PUSHGATEWAY_URL=" + pgw.URL}, []string{"lsf", "--dirs-only", "r:"}},
		"dry run is never recorded":  {[]string{"RCLONESHIM_PUSHGATEWAY_URL=" + pgw.URL}, []string{"sync", "a", "b", "--dry-run"}},
		"bad config does not matter": {[]string{"RCLONESHIM_INTERVAL=banana"}, []string{"version"}},
	}
	for name, c := range cases {
		got = nil
		h := newHarness(t, c.env...)
		code := Run("/some/path/rclone", c.args, h.env, "", Stdio{In: h.in, Out: h.outFile, Err: h.errFile})
		if got == nil || code != 126 {
			t.Fatalf("%s: execve not attempted (code %d)", name, code)
		}
		if got.argv[0] != "/some/path/rclone" || fmt.Sprintf("%q", got.argv[1:]) != fmt.Sprintf("%q", c.args) {
			t.Fatalf("%s: argv %q", name, got.argv)
		}
		// The environment is the caller's plus the recursion guard, nothing else.
		if want := append(append([]string(nil), h.env...), "RCLONESHIM_DEPTH=1"); fmt.Sprintf("%q", got.env) != fmt.Sprintf("%q", want) {
			t.Fatalf("%s: env %q", name, got.env)
		}
		if !strings.HasSuffix(got.path, "/bin/rclone") {
			t.Fatalf("%s: path %q", name, got.path)
		}
	}
	if n := len(pgw.all()); n != 0 {
		t.Fatalf("passthrough must never push, got %d requests", n)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	waitFor(t, path, func() bool { _, err := os.Stat(path); return err == nil })
}

func TestRun_ForegroundTTYDoesNotRelaySIGINT(t *testing.T) {
	old := foregroundTTY
	foregroundTTY = func() bool { return true }
	defer func() { foregroundTTY = old }()

	pgw := newPushRecorder(t)
	h := newHarness(t, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL, "FAKE_WAIT_SIGNAL=1")
	ready, signals := filepath.Join(h.dir, "ready"), filepath.Join(h.dir, "signals")
	h.env = append(h.env, "FAKE_READY="+ready, "FAKE_SIGNALS="+signals)

	done := make(chan int, 1)
	go func() { done <- h.run("sync", "a", "b") }()
	waitForFile(t, ready) // Run registered its handlers before starting the child
	_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	time.Sleep(300 * time.Millisecond)
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case code := <-done:
		if code != 128+int(syscall.SIGTERM) {
			t.Fatalf("exit %d, want 143", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	b, _ := os.ReadFile(signals)
	if got := strings.TrimSpace(string(b)); got != "terminated" {
		t.Fatalf("child received %q, want only SIGTERM (the terminal delivers Ctrl-C itself)", got)
	}
	if !strings.Contains(pgw.final(t).Body, "rclone_shim_exit_code 143\n") {
		t.Fatal("a signalled run must still be pushed")
	}
}

// ---- subprocess tests: the built shim, as a caller would run it ----

var (
	buildOnce sync.Once
	builtShim string
	buildErr  error
)

// shimBinary builds cmd/rclone-metrics-shim once and returns its path.
func shimBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "shimbin")
		if err != nil {
			buildErr = err
			return
		}
		builtShim = filepath.Join(dir, "rclone-metrics-shim")
		cmd := exec.Command("go", "build", "-o", builtShim, "../../cmd/rclone-metrics-shim")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return builtShim
}

type subprocess struct {
	t                      *testing.T
	dir                    string
	cmd                    *exec.Cmd
	record, ready, signals string
	stderr                 string
}

// startShim installs the built shim as "rclone" ahead of the fake on PATH and
// starts it in its own session (no controlling terminal).
func startShim(t *testing.T, args []string, extraEnv ...string) *subprocess {
	t.Helper()
	dir := shortTempDir(t)
	shimDir, fakeDir := filepath.Join(dir, "shim"), filepath.Join(dir, "fake")
	if err := Install(shimBinary(t), filepath.Join(shimDir, "rclone")); err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	if err := os.MkdirAll(fakeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(fakeDir, "rclone")); err != nil {
		t.Fatal(err)
	}
	s := &subprocess{t: t, dir: dir, record: filepath.Join(dir, "record.json"), ready: filepath.Join(dir, "ready"),
		signals: filepath.Join(dir, "signals"), stderr: filepath.Join(dir, "stderr")}
	errFile, err := os.Create(s.stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { errFile.Close() })
	s.cmd = exec.Command(filepath.Join(shimDir, "rclone"), args...)
	s.cmd.Env = append([]string{
		"PATH=" + shimDir + string(os.PathListSeparator) + fakeDir,
		"SHIM_FAKE_RCLONE=1", "FAKE_RECORD=" + s.record, "FAKE_READY=" + s.ready, "FAKE_SIGNALS=" + s.signals,
		"RCLONESHIM_INTERVAL=100ms",
	}, extraEnv...)
	s.cmd.Stderr = errFile
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })
	return s
}

// wait returns the status a shell would report for the shim.
func (s *subprocess) wait() int {
	s.t.Helper()
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		s.t.Fatal("shim did not exit")
	}
	if ws := s.cmd.ProcessState.Sys().(syscall.WaitStatus); ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return s.cmd.ProcessState.ExitCode()
}

func (s *subprocess) rec() fakeRecord {
	s.t.Helper()
	b, err := os.ReadFile(s.record)
	if err != nil {
		s.t.Fatalf("fake rclone did not run: %v", err)
	}
	var r fakeRecord
	_ = json.Unmarshal(b, &r)
	return r
}

// receivedSignals lists the signals the fake rclone recorded, in order.
func (s *subprocess) receivedSignals() []string {
	b, _ := os.ReadFile(s.signals)
	if strings.TrimSpace(string(b)) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestShim_PassthroughReplacesTheProcess(t *testing.T) {
	s := startShim(t, []string{"lsf", "remote:"}, "FAKE_EXIT=9") // no pushgateway configured
	if code := s.wait(); code != 9 {
		t.Fatalf("exit %d", code)
	}
	rec := s.rec()
	if rec.Pid != s.cmd.Process.Pid {
		t.Fatalf("rclone ran as pid %d but the shim was pid %d: the shim forked instead of exec'ing", rec.Pid, s.cmd.Process.Pid)
	}
	if strings.Join(rec.Args[1:], " ") != "lsf remote:" {
		t.Fatalf("args %q", rec.Args)
	}
}

func TestShim_ForwardsSignals(t *testing.T) {
	pgw := newPushRecorder(t)
	s := startShim(t, []string{"sync", "a", "b"}, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL, "FAKE_WAIT_SIGNAL=1")
	waitForFile(t, s.ready)
	if s.rec().Pid == s.cmd.Process.Pid {
		t.Fatal("wrapped mode must run rclone as a child")
	}
	for i, sig := range []syscall.Signal{syscall.SIGUSR1, syscall.SIGHUP, syscall.SIGINT, syscall.SIGUSR2, syscall.SIGQUIT} {
		_ = s.cmd.Process.Signal(sig)
		waitFor(t, sig.String(), func() bool { return len(s.receivedSignals()) == i+1 })
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	if code := s.wait(); code != 143 {
		t.Fatalf("exit %d, want 143 (128+SIGTERM)", code)
	}
	want := []string{syscall.SIGUSR1.String(), syscall.SIGHUP.String(), syscall.SIGINT.String(), syscall.SIGUSR2.String(), syscall.SIGQUIT.String(), syscall.SIGTERM.String()}
	if got := s.receivedSignals(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("child received %q, want %q", got, want)
	}
	if !strings.Contains(pgw.final(t).Body, "rclone_shim_exit_code 143\n") {
		t.Fatal("no push after a signalled run")
	}
}

func TestShim_IgnoredSIGHUPStaysIgnored(t *testing.T) {
	pgw := newPushRecorder(t)
	// `trap "" HUP` is what nohup does: the ignore is inherited across exec.
	s := startShim(t, nil, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL, "FAKE_WAIT_SIGNAL=1")
	_ = s.cmd.Process.Kill()
	_ = s.cmd.Wait()

	shimPath := s.cmd.Path
	cmd := exec.Command("/bin/sh", "-c", `trap "" HUP; exec "$0" sync a b`, shimPath)
	cmd.Env = s.cmd.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	os.Remove(s.ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	waitForFile(t, s.ready)
	_ = cmd.Process.Signal(syscall.SIGHUP)
	time.Sleep(300 * time.Millisecond)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	if got := s.receivedSignals(); len(got) != 1 || got[0] != syscall.SIGTERM.String() {
		t.Fatalf("child received %q: an ignored SIGHUP must not be relayed", got)
	}
}

func TestShim_SignalDuringPushKeepsChildStatus(t *testing.T) {
	pgw := newPushRecorder(t)
	pgw.hang = func(body string) bool { return strings.Contains(body, "rclone_shim_exit_code") }
	s := startShim(t, []string{"sync", "a", "b"}, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL, "RCLONESHIM_PUSH_TIMEOUT=60s", "FAKE_EXIT=0")
	waitFor(t, "the final push to be in flight", func() bool {
		for _, r := range pgw.all() {
			if strings.Contains(r.Body, "rclone_shim_exit_code") {
				return true
			}
		}
		return false
	})
	start := time.Now()
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	if code := s.wait(); code != 0 {
		t.Fatalf("exit %d, want the child's 0: a signal during the push must not change the status", code)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("shim took %s to give up the push", d)
	}
}

func TestShim_ClosedStderrDoesNotChangeStatus(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	s := startShim(t, nil)
	_ = s.cmd.Process.Kill()
	_ = s.cmd.Wait()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(s.cmd.Path, "sync", "a", "b")
	cmd.Env = append(s.cmd.Env, "RCLONESHIM_PUSHGATEWAY_URL=http://"+addr, "FAKE_EXIT=6")
	cmd.Stderr = w
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r.Close() // the reader went away: the shim's warning line hits EPIPE
	w.Close()
	_ = cmd.Wait()
	ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if ws.Signaled() || cmd.ProcessState.ExitCode() != 6 {
		t.Fatalf("status %v, want the child's exit 6", cmd.ProcessState)
	}
}

func TestCLI(t *testing.T) {
	bin := shimBinary(t)
	run := func(args ...string) (string, string, int) {
		cmd := exec.Command(bin, args...)
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		_ = cmd.Run()
		return out.String(), errb.String(), cmd.ProcessState.ExitCode()
	}
	if out, _, code := run("version"); code != 0 || strings.TrimSpace(out) != "dev" {
		t.Fatalf("version: %q, %d", out, code)
	}
	if _, errOut, code := run(); code != 2 || !strings.Contains(errOut, "Usage:") {
		t.Fatalf("no args: %q, %d", errOut, code)
	}
	if _, _, code := run("frobnicate"); code != 2 {
		t.Fatalf("unknown subcommand: %d", code)
	}
	if _, _, code := run("install"); code != 2 {
		t.Fatalf("install without dest: %d", code)
	}
	dest := filepath.Join(shortTempDir(t), "sbin", "rclone")
	if _, errOut, code := run("install", dest); code != 0 {
		t.Fatalf("install: %d %s", code, errOut)
	}
	info, err := os.Stat(dest)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("installed file: %v %v", info, err)
	}
	if _, _, code := run("install", "/proc/nonexistent/x/rclone"); code != 1 {
		t.Fatalf("failed install must exit 1, got %d", code)
	}
}

func TestRun_RecursionGuard(t *testing.T) {
	pgw := newPushRecorder(t)
	h := newHarness(t, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL, "RCLONESHIM_DEPTH=4")
	if code := h.run("sync", "a", "b"); code != 127 {
		t.Fatalf("exit %d, want 127", code)
	}
	if _, err := os.Stat(h.record); err == nil {
		t.Fatal("the child must not be started once the depth limit is reached")
	}
	if lines := shimLines(h.read(h.errFile)); len(lines) != 1 || !strings.Contains(lines[0], "recursion") {
		t.Fatalf("stderr %q", lines)
	}
}

// A shim started by another shim must not wrap again: one run, one push.
func TestRun_NestedShimPassesThrough(t *testing.T) {
	called := false
	old := execve
	execve = func(string, []string, []string) error { called = true; return syscall.ENOEXEC }
	defer func() { execve = old }()
	pgw := newPushRecorder(t)
	h := newHarness(t, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL, "RCLONESHIM_DEPTH=1")
	h.run("sync", "a", "b")
	if !called || len(pgw.all()) != 0 {
		t.Fatalf("execve=%v pushes=%d", called, len(pgw.all()))
	}
}

func TestRun_UnusableSocketDirStillRuns(t *testing.T) {
	old := probe
	probe = func(string) error { return syscall.EOPNOTSUPP }
	defer func() { probe = old }()
	pgw := newPushRecorder(t)
	h := newHarness(t, "RCLONESHIM_PUSHGATEWAY_URL="+pgw.URL, "FAKE_EXIT=2")
	if code := h.run("sync", "a", "b"); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if h.rec().HasAddr {
		t.Fatal("rclone must not be given a metrics address it cannot bind: it treats that as fatal")
	}
}
