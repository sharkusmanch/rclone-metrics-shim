# rclone-metrics-shim: Design and Implementation Plan

> **For agentic workers:** implement task-by-task with TDD. Steps use checkbox (`- [ ]`) syntax.
> Each task ends with `go vet ./... && go test ./...` green and a commit.

**Goal:** a drop-in program named `rclone` that runs the real rclone unchanged and, as a side
effect, pushes that run's Prometheus metrics to a Pushgateway.

**Architecture:** a single static Go binary. When invoked under the name `rclone` it finds the
real rclone further down `PATH`, starts it with identical arguments and inherited
stdin/stdout/stderr, turns on rclone's own metrics endpoint on a private unix socket through
the child's environment, polls that socket while the child runs, and after exit POSTs the last
sample plus its own exit-code/duration series to the Pushgateway. With no Pushgateway URL
configured, or for a subcommand that is not a transfer, it `exec`s the real rclone and is gone.

**Tech stack:** Go (module `go 1.22`, standard library only, no third-party dependencies),
GitHub Actions, GHCR, `scratch` container image.

---

## 1. Design

### 1.1 Why it exists

rclone can only *serve* metrics (`--metrics-addr`); it cannot push them. A nightly
`rclone sync` in a CronJob has exited long before anything scrapes it. The only tool that
bridged this (vshn/rclone-exporter) was archived in 2021 and needed rclone's RC API enabled by
the caller. Wrapping each call site in shell (`tee`, flag changes, exit-code plumbing) is the
alternative, and it is exactly where exit codes get lost.

### 1.2 Verified facts this design rests on (rclone v1.75.1)

- `RCLONE_METRICS_ADDR` in the environment, with no flags, enables `/metrics`.
- `RCLONE_METRICS_ADDR=unix:///path/m.sock` works; port `0` is useless (the port is not logged).
- The endpoint exposes 11 `rclone_*` series (`rclone_bytes_transferred_total`,
  `rclone_files_transferred_total`, `rclone_files_deleted_total`, `rclone_errors_total`,
  `rclone_fatal_error`, …) plus ~48 Go runtime / process series.
- The endpoint dies with the process. A 1 s poll on a 4 s sync saw 5 of 6 transfers and 0 of 1
  deletes: **the last sample is a lower bound, never exact.**
- A second rclone inheriting the same `RCLONE_METRICS_ADDR` fails to bind and logs `CRITICAL`.
  The variable must be set on the wrapped child only.
- The RC API (`core/stats`) returned 401 even with `RCLONE_RC_NO_AUTH=true`; not used.

### 1.3 Transparency contract

These are the properties a caller may rely on. Each has a test.

1. Arguments reach the real rclone byte-for-byte, in order.
2. stdin, stdout and stderr are the caller's own file descriptors (inherited, not piped).
3. The exit status is the real rclone's. Death by signal `N` is reported as `128+N`.
4. `SIGINT`, `SIGTERM`, `SIGHUP`, `SIGQUIT`, `SIGUSR1`, `SIGUSR2` are forwarded to the child.
5. Nothing the shim does (socket, scrape, push, Pushgateway down, slow or erroring) changes
   1–4. A push is bounded by a timeout. The shim's only own output is a single
   `rclone-metrics-shim: …` line on stderr when something of its own fails.
6. Unconfigured (`RCLONESHIM_PUSHGATEWAY_URL` unset) it is `execve` of the real binary.

### 1.4 Two personalities, chosen by `argv[0]`

| Invoked as | Behaviour |
|---|---|
| `rclone` (basename, any directory) | shim mode, everything below |
| anything else (`rclone-metrics-shim`) | management CLI: `install <dest>`, `version`, `help` |

`install <dest>` copies the running executable to `<dest>` with mode `0755` (creating parent
directories). This lets the published image be `scratch`: an init container runs
`/rclone-metrics-shim install /shim/rclone` into a shared `emptyDir`, which the job container
mounts at a directory early on `PATH`. No shell or `cp` in the image.

### 1.5 Configuration (environment only)

Prefix is `RCLONESHIM_`, deliberately **not** `RCLONE_`: rclone gives meaning to `RCLONE_*`
(flags) and `RCLONE_CONFIG_*` (remotes).

| Variable | Default | Meaning |
|---|---|---|
| `RCLONESHIM_PUSHGATEWAY_URL` | unset → pure passthrough | Base URL, e.g. `http://pushgateway:9091` |
| `RCLONESHIM_INSTANCE` | unset → no `instance` in the grouping key | Name of this sync job |
| `RCLONESHIM_JOB` | `rclone` | Pushgateway `job` grouping label |
| `RCLONESHIM_COMMANDS` | `sync,copy,move` | Subcommands that are wrapped; others pass through |
| `RCLONESHIM_INTERVAL` | `1s` | Scrape interval (Go duration, min `100ms`) |
| `RCLONESHIM_PUSH_TIMEOUT` | `10s` | Total budget for the push |
| `RCLONESHIM_REAL` | unset → search `PATH` | Absolute path of the real rclone |

An unparseable value falls back to the default and prints one warning line; it never aborts
the run.

### 1.6 Finding the real rclone

`RCLONESHIM_REAL` if set. Otherwise each `PATH` entry in order: the first regular, executable
file named `rclone` that is **not** the same file as the running executable
(`os.SameFile` against `os.Executable()` after `EvalSymlinks`). Not found → message on stderr,
exit `127` (the shell's "command not found").

### 1.7 Deciding whether to wrap

Wrap only when all hold: a Pushgateway URL is set; the subcommand is in
`RCLONESHIM_COMMANDS`; the caller has not already configured a metrics endpoint
(`RCLONE_METRICS_ADDR` set, or any argument `--metrics-addr` / `--metrics-addr=…`). In the
last case the shim still wraps, but **does not scrape**: it pushes only its own series.

Subcommand detection scans the arguments left to right:

- An argument starting with `-` is a flag: skip it. `--` ends flag parsing: stop, no command.
- A non-flag argument that is in the wrapped set → wrap.
- A non-flag argument not in the set, whose predecessor is a flag without `=`, may be that
  flag's value (`--config /x sync`): keep scanning.
- Any other non-flag argument is the real subcommand (`lsf`, `purge`, …) → passthrough.

Known imprecision, accepted and documented: `rclone -v purge sync` (a path literally named
`sync`, with a boolean flag before the subcommand) is treated as wrapped. The cost is one
unneeded push.

### 1.8 Scraping

- Socket at `<tmpdir>/m.sock` where `<tmpdir>` is a fresh `os.MkdirTemp("", "rclone-shim-")`,
  removed on exit. `sockaddr_un` paths are limited (~104 bytes on macOS, 108 on Linux): if the
  path is longer than 100 bytes, do not scrape (push own series only) and warn.
- Until the first successful scrape, retry every `100ms` (the socket appears a moment after
  start); afterwards every `RCLONESHIM_INTERVAL`. Each request has a `2s` timeout.
- Keep only the most recent successful body, and when it was taken.
- Filter: keep sample lines whose metric name starts with `rclone_`, and the `# HELP` /
  `# TYPE` lines for those names. Drop everything else (Go runtime, process, promhttp).

### 1.9 Pushing

`POST {url}/metrics/job/{job}[/instance/{instance}]`, `Content-Type: text/plain; version=0.0.4`.

- **POST, not PUT.** POST replaces only the metric names present in the body. On a failed run
  the shim omits `rclone_shim_last_success_timestamp_seconds`, so the previous value survives.
  PUT would delete it and the shim has no way to know the old value.
- Grouping values containing `/` (or empty) use the Pushgateway's `label@base64/<urlsafe-b64>`
  form; otherwise `url.PathEscape`.
- A non-2xx response or transport error prints one warning line. One attempt, no retries.

Series added by the shim (all gauges):

| Series | Value |
|---|---|
| `rclone_shim_exit_code` | exit status reported to the caller |
| `rclone_shim_duration_seconds` | wall-clock run time of the child |
| `rclone_shim_last_run_timestamp_seconds` | unix time at child exit |
| `rclone_shim_last_success_timestamp_seconds` | same, **only when exit code is 0** |
| `rclone_shim_scrape_success` | `1` if at least one scrape succeeded, else `0` |
| `rclone_shim_last_scrape_age_seconds` | child exit time − last successful scrape; omitted when none |
| `rclone_shim_info{version="…",command="sync"}` | `1` |

Documented caveats for consumers:

- rclone's `*_total` series restart at zero every run. Compare raw values; `increase()` and
  `rate()` are meaningless across runs.
- Transfer series are a lower bound by up to one interval (§1.2). `rclone_shim_last_scrape_age_seconds`
  says how stale. Exit code, duration and timestamps are exact.
- When a run produced no scrape (`rclone_shim_scrape_success 0`, e.g. rclone failed at once),
  the `rclone_*` series in the group are **the previous run's**.
- Pushgateway groups never expire. A retired job's group must be deleted by hand.

### 1.10 Out of scope (YAGNI)

Retries of the push; exact final totals via `--use-json-log` re-rendering; TLS or auth to the
Pushgateway beyond what is in the URL (`http://user:pass@host`); Windows; release binaries
(image and `go install` only).

---

## 2. File structure

```
cmd/rclone-metrics-shim/main.go     # argv[0] dispatch, os.Exit(code) only
internal/shim/config.go             # Config, LoadConfig(env) -> (Config, warnings)
internal/shim/detect.go             # WrapDecision(args, cfg, env) -> (command string, wrap, scrape bool)
internal/shim/locate.go             # FindReal(pathEnv, self, override) (string, error)
internal/shim/metrics.go            # FilterRclone([]byte) []byte ; RenderShim(Result) []byte
internal/shim/push.go               # PushURL(cfg) string ; Push(ctx, cfg, body) error
internal/shim/scrape.go             # Scraper: Start(socket, interval) / Stop() -> (body, at, ok)
internal/shim/run.go                # Run(args, env, stdio) int   — orchestration
internal/shim/install.go            # Install(dest) error
internal/shim/*_test.go             # unit tests per file
internal/shim/integration_test.go   # fake rclone + fake pushgateway, end to end through Run
internal/shim/e2e_test.go           # //go:build e2e — real rclone binary from $RCLONE_BIN
Dockerfile                          # golang builder -> scratch
.github/workflows/ci.yml            # vet, test -race, e2e against pinned real rclone, image build (no push)
.github/workflows/release.yml       # tag v* -> test, multi-arch image to GHCR, provenance attestation
README.md  renovate.json  .gitignore
```

`version` is a package-level `var Version = "dev"` in `internal/shim`, set with
`-ldflags "-X …/internal/shim.Version=$VERSION"`.

## 3. Global constraints

- Go standard library only. `go.mod` says `go 1.22`.
- `CGO_ENABLED=0`; the binary must run in `scratch` and under any uid, read-only root filesystem
  (it writes only under `os.TempDir()`; if that fails it runs the child without scraping).
- Unix only (`//go:build unix` where syscalls are used). Linux amd64 + arm64 images.
- The shim must never exit non-zero for its own reasons while the child ran: the caller's
  exit status is the child's, always. Only "real rclone not found" (127) and `install`
  failures (1) are the shim's own.
- No output on stdout, ever. stderr only for the single-line warnings in §1.3(5).

## 4. Review focus

Conditions the design implies that are most likely to bite, each pinned by a named test:

1. **Pushgateway hangs** (accepts the connection, never answers): the child's exit status is
   still returned, within `PUSH_TIMEOUT`. → `TestRun_PushHangsIsBoundedAndTransparent`
2. **rclone exits before any scrape** (config error, instant failure): exit code still
   pushed, `scrape_success 0`, no `rclone_*` series in the body. → `TestRun_NoScrape`
3. **Child killed by a signal / shim receives SIGTERM** (pod deadline): signal forwarded,
   status `128+N`, push still attempted. → `TestRun_ForwardsSIGTERM`
4. **Temp dir not writable** (read-only rootfs, no `/tmp`): child still runs with identical
   args and status; no scrape. → `TestRun_NoTempDirStillRuns`
5. **Shim finds itself on PATH twice** (installed in two dirs, or symlinked): must not
   recurse into itself. → `TestFindReal_SkipsSelfAndSymlinkToSelf`

---

## 5. Tasks

### Task 1: scaffold, config, version

**Files:** `go.mod`, `.gitignore`, `cmd/rclone-metrics-shim/main.go`, `internal/shim/config.go`,
`internal/shim/config_test.go`

**Produces:**
```go
var Version = "dev"
type Config struct {
    PushgatewayURL string        // "" = passthrough
    Job, Instance  string
    Commands       map[string]bool
    Interval       time.Duration
    PushTimeout    time.Duration
    Real           string
}
// env is os.Environ()-style "K=V". Never fails; bad values fall back and add a warning.
func LoadConfig(env []string) (Config, []string)
```

- [ ] Tests: defaults; every variable overridden; `RCLONESHIM_INTERVAL=banana` → `1s` + one
      warning; `RCLONESHIM_INTERVAL=1ms` → clamped to `100ms` + warning;
      `RCLONESHIM_COMMANDS=" sync , copyto "` trims and yields `{sync, copyto}`; empty
      `RCLONESHIM_COMMANDS=` → default set; trailing `/` stripped from the URL.
- [ ] Implement; `main.go` only dispatches on `filepath.Base(os.Args[0]) == "rclone"`.
- [ ] `go vet ./... && go test ./...`; commit.

### Task 2: locate the real rclone

**Files:** `internal/shim/locate.go`, `locate_test.go`

**Produces:** `func FindReal(pathEnv, self, override string) (string, error)`;
`var ErrNotFound`.

- [ ] Tests (temp dirs with real files): first match wins; skips a non-executable `rclone`;
      skips a directory named `rclone`; `TestFindReal_SkipsSelfAndSymlinkToSelf` (self in dir
      A, symlink to self in dir B, real in dir C → C); override returned as-is without PATH
      search; empty PATH entries ignored; nothing found → `ErrNotFound`.
- [ ] Implement with `os.Stat` + `os.SameFile`; commit.

### Task 3: wrap decision

**Files:** `internal/shim/detect.go`, `detect_test.go`

**Produces:** `func WrapDecision(args []string, cfg Config, env []string) (command string, wrap, scrape bool)`

- [ ] Table test, one row per rule in §1.7:

| args | env | → command, wrap, scrape |
|---|---|---|
| `sync a b` | – | `sync`, true, true |
| `--config /x sync a b` | – | `sync`, true, true |
| `--config=/x -v copy a b` | – | `copy`, true, true |
| `lsf --dirs-only r:` | – | `""`, false, false |
| `purge r:x` | – | `""`, false, false |
| `-v lsf sync` | – | `""`... **see note** |
| `version` | – | false |
| (none) | – | false |
| `-- sync` | – | false |
| `sync a b --metrics-addr :9` | – | `sync`, true, **false** |
| `sync a b --metrics-addr=:9` | – | `sync`, true, false |
| `sync a b` | `RCLONE_METRICS_ADDR=:9` | `sync`, true, false |
| `sync a b` | URL unset in cfg | false |

      Note on `-v lsf sync`: `lsf` follows a flag so it may be a value; scanning continues and
      `sync` matches. This is the documented imprecision: assert `sync, true, true` and name
      the test `TestWrapDecision_KnownImprecision` so nobody "fixes" it by accident.
- [ ] Implement; commit.

### Task 4: metrics text

**Files:** `internal/shim/metrics.go`, `metrics_test.go`,
`internal/shim/testdata/rclone-1.75.1.metrics` (a real scrape, captured from v1.75.1)

**Produces:**
```go
func FilterRclone(scrape []byte) []byte
type Result struct {
    Command    string
    ExitCode   int
    Start, End time.Time
    Scraped    bool
    LastScrape time.Time
}
func RenderShim(r Result) []byte
```

- [ ] Tests: filtering the golden file keeps exactly the `rclone_*` samples and their
      HELP/TYPE lines and none starting `go_`, `process_`, `promhttp_`; a labelled sample
      (`rclone_x{a="b"} 1`) is kept; input without trailing newline; empty input → empty.
      `RenderShim`: success includes `last_success`; exit 7 omits it; `Scraped=false` emits
      `scrape_success 0` and omits `last_scrape_age`; label values in `rclone_shim_info`
      escape `\`, `"` and newline; output ends with `\n`; every sample has HELP and TYPE.
- [ ] Implement; commit.

### Task 5: push

**Files:** `internal/shim/push.go`, `push_test.go`

**Produces:** `func PushURL(cfg Config) string`; `func Push(ctx context.Context, cfg Config, body []byte) error`

- [ ] Tests: URL with and without instance; `instance="a/b"` →
      `…/instance@base64/YS9i`; a space is path-escaped; `httptest` server asserts method
      `POST`, content type and body; 400 response → error containing the status and the first
      line of the response body; context deadline against a handler that blocks → error within
      the deadline.
- [ ] Implement with a dedicated `http.Client` (no global state); commit.

### Task 6: scraper

**Files:** `internal/shim/scrape.go`, `scrape_test.go`

**Produces:**
```go
type Scraper struct{ /* … */ }
func StartScraper(socket string, interval time.Duration) *Scraper
func (s *Scraper) Stop() (body []byte, at time.Time, ok bool)   // idempotent, waits for the loop
```

- [ ] Tests (a test HTTP server on a unix socket in `t.TempDir()`): returns the **latest**
      body when the server changes its answer; server appears 300 ms after start → still
      scraped (fast initial retry); server never appears → `ok=false`, `Stop` returns promptly;
      server returns 500 → not stored; `Stop` twice is safe; no goroutine left running
      (loop exit observed through a done channel).
- [ ] Implement: `http.Transport{DialContext: unix dial}`, request URL `http://rclone/metrics`,
      2 s per-request timeout; commit.

### Task 7: run (orchestration) and integration tests

**Files:** `internal/shim/run.go`, `integration_test.go`, `cmd/rclone-metrics-shim/main.go`

**Produces:**
```go
type Stdio struct{ In, Out, Err *os.File }
// Returns the exit status for the caller. In passthrough mode it does not return (execve).
func Run(args []string, env []string, self string, stdio Stdio) int
```

Flow: `LoadConfig` → `FindReal` (127 on failure) → `WrapDecision` → passthrough
(`syscall.Exec(real, append([]string{"rclone"}, args...), env)`) **or** wrapped: temp dir +
socket (skip scraping if unavailable) → `exec.Cmd` with inherited stdio and
`RCLONE_METRICS_ADDR=unix://…` appended to the child's env only → `signal.Notify` +
forwarding goroutine → `Start` → scraper → `Wait` → stop scraper, stop signal relay → build
body (`FilterRclone` + `RenderShim`) → `Push` under `PushTimeout` → remove temp dir → return
status.

The fake rclone is the test binary itself re-executed (`TestMain` checks
`SHIM_FAKE_RCLONE=1`): it records its argv and selected env to a file, optionally serves a
fixed metrics body on the socket named by `RCLONE_METRICS_ADDR`, writes markers to
stdout/stderr, sleeps, and exits with a requested code or waits for a signal. Tests install
it as `rclone` in a temp `PATH` dir via symlink/copy.

- [ ] `TestRun_PassesArgsAndExitCode` — args with spaces, empty string and unicode arrive
      identically; exit codes 0, 1, 7 returned.
- [ ] `TestRun_StdioInherited` — child's stdout/stderr markers land in the files given as
      `Stdio`; stdin content is readable by the child.
- [ ] `TestRun_PushesScrapeAndShimSeries` — fake Pushgateway receives one POST at
      `/metrics/job/rclone/instance/t`, containing `rclone_files_transferred_total 5`,
      `rclone_shim_exit_code 0`, `last_success`, and **no** `go_` series.
- [ ] `TestRun_FailureOmitsLastSuccess` — exit 7 → body has `rclone_shim_exit_code 7`, no
      `last_success`.
- [ ] `TestRun_NoScrape` — fake exits immediately without serving → `scrape_success 0`.
- [ ] `TestRun_PushgatewayDownIsTransparent` — URL points at a closed port → status is the
      child's, exactly one warning line on stderr.
- [ ] `TestRun_PushHangsIsBoundedAndTransparent` — handler blocks; `PUSH_TIMEOUT=300ms` →
      `Run` returns the child's status in under 2 s.
- [ ] `TestRun_ForwardsSIGTERM` — child waits for a signal; test sends `SIGTERM` to its own
      process; child records it and exits by re-raising → status `143`; a push still arrives.
- [ ] `TestRun_NoTempDirStillRuns` — `TMPDIR` set to a non-existent path → child runs, status
      and args correct, `scrape_success 0`.
- [ ] `TestRun_ChildEnvOnly` — child sees `RCLONE_METRICS_ADDR=unix://…`; the shim's own
      process environment is unchanged afterwards.
- [ ] `TestRun_CallerMetricsAddrNotOverridden` — with `RCLONE_METRICS_ADDR=:1` in env the
      child sees that exact value.
- [ ] `TestRun_RealNotFound` — empty PATH → 127 and a message on stderr.
- [ ] `TestPassthrough_Execs` — run the built shim as a subprocess with no URL: the fake
      reports its **parent pid equals the test's child pid** (i.e. the shim was replaced, not
      a parent), and args/exit code match.
- [ ] `go test -race ./...` green; commit.

### Task 8: install subcommand and management CLI

**Files:** `internal/shim/install.go`, `install_test.go`, `cmd/rclone-metrics-shim/main.go`

- [ ] Tests: `Install` creates parent dirs, mode `0755`, content identical to the source,
      overwrites an existing file, works when dest is on a different filesystem (plain copy,
      not rename/hardlink); CLI: `version` prints `Version`; unknown subcommand → usage on
      stderr, exit 2; `install` with no dest → exit 2.
- [ ] Implement (write to `dest.tmp`, `chmod`, `rename`); commit.

### Task 9: end-to-end against a real rclone

**Files:** `internal/shim/e2e_test.go` (`//go:build e2e`)

- [ ] `TestE2E_RealRcloneSync`: needs `RCLONE_BIN`. Builds the shim, installs it as `rclone`
      ahead of a dir containing the real binary, creates a source tree (a 20 MiB file and five
      small files) and a destination with one stale file, runs
      `rclone sync src dst --bwlimit 10M` through the shim against an `httptest` Pushgateway.
      Asserts: exit 0; destination equals source; pushed body has `rclone_shim_exit_code 0`,
      `rclone_shim_scrape_success 1`, and `rclone_bytes_transferred_total` > 0.
- [ ] `TestE2E_RealRcloneLsfIsPassthrough`: `rclone lsf src` through the shim → listing on
      stdout, no request reaches the fake Pushgateway.
- [ ] Run locally if an rclone binary is available, otherwise in CI; commit.

### Task 10: Dockerfile, CI, release, README

**Files:** `Dockerfile`, `.github/workflows/ci.yml`, `.github/workflows/release.yml`,
`README.md`, `renovate.json`

- [ ] `Dockerfile`: `golang:1.25-alpine` builder with `--platform=$BUILDPLATFORM`,
      `CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X …Version=$VERSION"`;
      final stage `scratch`, binary at `/rclone-metrics-shim`, `USER 65532:65532`,
      `ENTRYPOINT ["/rclone-metrics-shim"]`.
- [ ] `ci.yml` (push + PR): `go vet`, `go test -race ./...`; e2e job downloads rclone
      `v1.75.1` from `downloads.rclone.org` (pinned, Renovate-trackable), sets `RCLONE_BIN`,
      runs `go test -tags e2e ./...`; image build without push.
- [ ] `release.yml` (tags `v*` only, same shape as the owner's other repos): re-run tests →
      buildx `linux/amd64,linux/arm64` → push `ghcr.io/sharkusmanch/rclone-metrics-shim`
      with semver tags → `actions/attest-build-provenance` with `push-to-registry: true`.
- [ ] `README.md`: what/why, the transparency contract, configuration table, the metrics table
      and the four caveats from §1.9, Kubernetes init-container example, plain cron example,
      example alert rules.
- [ ] Commit; push; CI green.

### Task 11: release v0.1.0

- [ ] Tag `v0.1.0`, push the tag, wait for the release workflow.
- [ ] `gh attestation verify oci://ghcr.io/sharkusmanch/rclone-metrics-shim:v0.1.0 --owner sharkusmanch`.
- [ ] Confirm the image is pullable anonymously (the GHCR package must be **public**; a first
      publish may default to private and that setting is only changeable in the GitHub UI).

### Task 12: adopt in the cluster (repo `home-ops-public`)

Pilot on one job, verify, then the rest. All in
`clusters/home/apps/tools/rclone-pcloud/helm-release.yaml` unless stated.

- [ ] Kyverno: add a `rclone-metrics-shim` rule to `policy-verify-image-attestations.yaml`
      (subject `^https://github\.com/sharkusmanch/rclone-metrics-shim/\.github/workflows/release\.yml@refs/tags/v.+$`).
- [ ] `syncthing-sync` controller: init container `shim-install` (image
      `ghcr.io/sharkusmanch/rclone-metrics-shim:0.1.0`, args `install /shim/rclone`, same
      hardened securityContext); `emptyDir` `shim` mounted at `/shim` in the init container and
      at `/usr/local/sbin` in the job container (first on the image's `PATH`; the directory
      does not exist in `rclone/rclone`, so nothing is hidden); env
      `RCLONESHIM_PUSHGATEWAY_URL=http://prometheus-pushgateway.monitoring.svc.cluster.local:9091`
      and `RCLONESHIM_INSTANCE=syncthing-sync`.
- [ ] NetworkPolicy: egress to the pushgateway pod on TCP 9091 (copy of the `db-backup` rule).
- [ ] Verify the repo's `tests/cronjob-scripts/rclone-exit-codes.sh` still passes; render the
      chart; server-side dry run.
- [ ] Merge, reconcile, trigger the CronJob by hand. Verify: job log is byte-for-byte the
      usual rclone output with no shim lines; exit code 0; Prometheus has
      `rclone_shim_exit_code{instance="syncthing-sync"} 0` and `rclone_*` series.
- [ ] Alerts in `backup-prometheus-rule.yaml`: `RcloneSyncFailed`
      (`rclone_shim_exit_code != 0`, 5m, names `{{ $labels.instance }}`).
- [ ] Roll the same three changes to `backup-sync`, `immich-photos-sync`, `books-sync`
      (instances named after the controllers); update `README.md` there.

## 6. Self-review

- Spec coverage: every row of §1.3 maps to a Task 7 test; §1.5 → Task 1; §1.6 → Task 2;
  §1.7 → Task 3; §1.8 → Task 6; §1.9 → Tasks 4, 5, 7; §1.4 → Task 8.
- All five review-focus conditions have a named test in Tasks 2 and 7.
- Names are consistent across tasks: `Config`, `LoadConfig`, `FindReal`, `WrapDecision`,
  `FilterRclone`, `RenderShim`, `Result`, `PushURL`, `Push`, `StartScraper`, `Scraper.Stop`,
  `Run`, `Stdio`, `Install`, `Version`.
