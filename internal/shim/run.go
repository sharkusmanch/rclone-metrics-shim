package shim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

const (
	exitNotFound  = 127 // same as a shell's "command not found"
	exitCannotRun = 126
	// sockaddr_un holds ~104 bytes on macOS and 108 on Linux.
	maxSocketPath = 100
	// depthEnv counts shims on the way to the real rclone.
	depthEnv = "RCLONESHIM_DEPTH"
	maxDepth = 4
)

// probeSocket checks that a unix socket can be bound at path.
func probeSocket(path string) error {
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	l.Close() // closing a unix listener removes its socket file
	return nil
}

// forwarded are the signals relayed to the real rclone.
var forwarded = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2}

// execve is replaced in tests; passthrough mode replaces the process.
var execve = syscall.Exec

// probe is replaced in tests.
var probe = probeSocket

// foregroundTTY is replaced in tests.
var foregroundTTY = inForegroundOfTTY

// Stdio are the caller's standard streams, handed to the child unchanged.
type Stdio struct{ In, Out, Err *os.File }

// warn writes the shim's only kind of output: one line on stderr. Errors are
// ignored on purpose; a closed stderr must not affect the run.
func warn(stdio Stdio, format string, a ...any) {
	_, _ = fmt.Fprintf(stdio.Err, "rclone-metrics-shim: "+format+"\n", a...)
}

// Run executes the real rclone and returns the exit status to hand back to the
// caller. arg0 is the name the shim was invoked under and is passed on as the
// child's argv[0]; self is the path of the running shim, used to avoid finding
// itself on PATH. In passthrough mode Run does not return.
func Run(arg0 string, args, env []string, self string, stdio Stdio) int {
	// A warning written to a closed stderr pipe would otherwise kill the shim
	// with SIGPIPE. Notify (not Ignore): a caught signal is reset to its default
	// in the child and across execve, an ignored one would be inherited.
	pipe := make(chan os.Signal, 1)
	signal.Notify(pipe, syscall.SIGPIPE)
	defer signal.Stop(pipe)

	cfg, warnings := LoadConfig(env)
	pathEnv, _ := lookupEnv(env, "PATH")
	real, err := FindReal(pathEnv, self, cfg.Real)
	if err != nil {
		warn(stdio, "%v", err)
		return exitNotFound
	}

	// Depth guard: whatever PATH looks like, a shim that keeps finding shims
	// must stop instead of spinning. Nested shims also never wrap twice.
	depth := 0
	if v, ok := lookupEnv(env, depthEnv); ok {
		depth, _ = strconv.Atoi(v)
	}
	if depth >= maxDepth {
		warn(stdio, "recursion: %s resolves to the shim again; set RCLONESHIM_REAL", real)
		return exitNotFound
	}
	env = append(append([]string(nil), env...), depthEnv+"="+strconv.Itoa(depth+1))

	command, wrap, scrape := WrapDecision(args, cfg, env)
	argv := append([]string{arg0}, args...)
	if !wrap || depth > 0 {
		err := execve(real, argv, env)
		warn(stdio, "exec %s: %v", real, err)
		return exitCannotRun
	}

	for _, w := range warnings {
		warn(stdio, "%s", w)
	}

	childEnv := env
	socket := ""
	if scrape {
		if dir, err := os.MkdirTemp("", "rclone-shim-"); err != nil {
			warn(stdio, "no temp dir, transfer metrics unavailable: %v", err)
		} else {
			defer os.RemoveAll(dir)
			// rclone treats a metrics address it cannot bind as fatal, so prove
			// a socket can be created here before asking it to.
			if s := filepath.Join(dir, "m.sock"); len(s) > maxSocketPath {
				warn(stdio, "temp dir path too long for a unix socket, transfer metrics unavailable")
			} else if err := probe(filepath.Join(dir, "p.sock")); err != nil {
				warn(stdio, "cannot create a unix socket in %s, transfer metrics unavailable: %v", dir, err)
			} else {
				socket = s
				childEnv = append(append([]string(nil), env...), "RCLONE_METRICS_ADDR=unix://"+socket)
			}
		}
	}

	// Pdeathsig is tied to the OS thread that forks the child.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	cmd := &exec.Cmd{
		Path:        real,
		Args:        argv,
		Env:         childEnv,
		Stdin:       stdio.In,
		Stdout:      stdio.Out,
		Stderr:      stdio.Err,
		SysProcAttr: childSysProcAttr(),
	}

	// Relay signals, except those the caller arranged to ignore (nohup):
	// Notify would turn an inherited "ignore" into a delivery to the child.
	sigs := make(chan os.Signal, 16)
	for _, s := range forwarded {
		if !signal.Ignored(s) {
			signal.Notify(sigs, s)
		}
	}
	defer signal.Stop(sigs)

	start := time.Now()
	if err := cmd.Start(); err != nil {
		warn(stdio, "start %s: %v", real, err)
		return exitCannotRun
	}

	// Announce the start without delaying rclone. A group whose start is newer
	// than its last run is still running or was killed before it could report.
	startCtx, cancelStart := context.WithTimeout(context.Background(), cfg.PushTimeout)
	startPushed := make(chan struct{})
	go func() {
		defer close(startPushed)
		_ = Push(startCtx, cfg, RenderStart(start))
	}()

	var scraper *Scraper
	if socket != "" {
		scraper = StartScraper(socket, cfg.Interval)
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var waitErr error
wait:
	for {
		select {
		case s := <-sigs:
			if (s == syscall.SIGINT || s == syscall.SIGQUIT) && foregroundTTY() {
				continue // the terminal already delivered it to the child
			}
			_ = cmd.Process.Signal(s)
		case waitErr = <-waited:
			break wait
		}
	}
	end := time.Now()
	code := exitStatus(cmd.ProcessState, waitErr)

	cancelStart() // the final push supersedes it, and must not race it
	<-startPushed

	result := Result{Command: command, ExitCode: code, Start: start, End: end}
	var body []byte
	if scraper != nil {
		var scraped []byte
		scraped, result.LastScrape, result.Scraped = scraper.Stop()
		body = FilterRclone(scraped)
	}
	body = append(body, RenderShim(result)...)

	// A signal that arrives now is for a process that has already finished:
	// abandon the push rather than delay or change the exit status.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.PushTimeout)
	defer cancel()
	pushed := make(chan error, 1)
	go func() { pushed <- Push(ctx, cfg, body) }()
	for {
		select {
		case err := <-pushed:
			if err != nil {
				warn(stdio, "push failed: %v", err)
			}
			return code
		case s := <-sigs:
			if s == syscall.SIGUSR1 || s == syscall.SIGUSR2 {
				continue // meant for rclone, which is gone; not a request to stop
			}
			cancel()
			warn(stdio, "push abandoned on %v", s)
			return code
		}
	}
}

// exitStatus maps the child's fate to the status a shell would report.
func exitStatus(state *os.ProcessState, waitErr error) int {
	if state != nil {
		if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return state.ExitCode()
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode()
	}
	return exitCannotRun
}
