# rclone-metrics-shim

A transparent wrapper for [rclone](https://rclone.org) that pushes Prometheus metrics for each
run to a [Pushgateway](https://github.com/prometheus/pushgateway).

rclone can serve metrics (`--metrics-addr`) but cannot push them, so a nightly `rclone sync`
from cron or a Kubernetes CronJob has exited long before anything scrapes it. This shim is
installed under the name `rclone`, ahead of the real one on `PATH`. Your scripts do not change:

```sh
rclone sync /data remote:backup --transfers 8   # same command, same output, same exit code
```

and afterwards the Pushgateway holds that run's exit code, duration, timestamps and rclone's
own transfer counters.

## What "transparent" means

The shim is built around a contract, and every line of it is covered by a test:

1. Arguments reach the real rclone byte-for-byte, including `argv[0]`.
2. stdin, stdout and stderr are the caller's own file descriptors, inherited rather than piped.
3. The exit status is the real rclone's. Death by signal `N` is reported as `128+N`.
4. `SIGINT`, `SIGTERM`, `SIGHUP`, `SIGQUIT`, `SIGUSR1` and `SIGUSR2` are relayed to rclone.
   A signal the caller ignored (`nohup`) stays ignored, and a Ctrl-C from a terminal is not
   delivered twice.
5. Nothing the shim does can change 1–4. A Pushgateway that is down, slow or returning errors
   costs at most `RCLONESHIM_PUSH_TIMEOUT` at exit and one line on stderr.
6. With no Pushgateway configured, or for any rclone command that is not a transfer
   (`lsf`, `purge`, `config`, …), the shim `exec`s the real rclone and is gone.

What the shim does change, when it wraps a run:

- rclone runs as a child of the shim rather than in its place.
- The child gets `RCLONE_METRICS_ADDR=unix://<tmpdir>/m.sock` in its environment, unless you
  configured a metrics address yourself.
- rclone's environment gains `RCLONESHIM_DEPTH`, a guard that stops a shim which finds
  another shim from looping. A shim started by a shim never wraps a second time.
- At `-vv`, rclone logs one extra line: `DEBUG : Setting --metrics-addr … from environment variable`.
- If the shim is killed with `SIGKILL`, rclone is killed too on Linux. On other systems it
  keeps running, and the shim's temp directory (`$TMPDIR/rclone-shim-*`) is left behind.

## Install

**Container image** (`linux/amd64`, `linux/arm64`, `scratch`-based):

```
ghcr.io/sharkusmanch/rclone-metrics-shim:<version>
```

**From source:**

```sh
go install github.com/sharkusmanch/rclone-metrics-shim/cmd/rclone-metrics-shim@latest
rclone-metrics-shim install /usr/local/sbin/rclone   # any directory before the real rclone on PATH
```

The program decides what to be from its own name: called `rclone` it is the shim, called
anything else it is a small management CLI (`install <dest>`, `version`, `help`).

## Configuration

Environment variables only. The prefix is `RCLONESHIM_`, not `RCLONE_`, because rclone gives
meaning to `RCLONE_*`.

| Variable | Default | Meaning |
|---|---|---|
| `RCLONESHIM_PUSHGATEWAY_URL` | unset: the shim does nothing | Base URL, e.g. `http://pushgateway:9091` |
| `RCLONESHIM_INSTANCE` | unset | Name of this sync job; becomes the `instance` grouping label |
| `RCLONESHIM_JOB` | `rclone` | The `job` grouping label |
| `RCLONESHIM_COMMANDS` | `sync,copy,move,copyto,moveto,bisync` | rclone commands that are wrapped |
| `RCLONESHIM_INTERVAL` | `1s` | How often rclone's metrics are sampled (minimum `100ms`) |
| `RCLONESHIM_PUSH_TIMEOUT` | `10s` | Upper bound on each push |
| `RCLONESHIM_REAL` | unset: search `PATH` | Absolute path of the real rclone |

A value that cannot be parsed falls back to its default with a warning; it never stops the run.

**Give every sync job its own `RCLONESHIM_INSTANCE`**, and wrap one transfer per instance. The
Pushgateway keeps one set of series per `job`/`instance`; two transfers sharing a group
overwrite each other.

Runs that are never recorded, whatever the configuration: `--dry-run` / `-n`,
`--interactive` / `-i`, and `--help`.

## Metrics

Pushed with `POST` to `{url}/metrics/job/{job}/instance/{instance}`.

From the shim (gauges):

| Series | Meaning |
|---|---|
| `rclone_shim_last_start_timestamp_seconds` | When the last run started. Pushed as the run begins. |
| `rclone_shim_last_run_timestamp_seconds` | When the last run ended |
| `rclone_shim_last_success_timestamp_seconds` | When the last run that exited `0` ended |
| `rclone_shim_exit_code` | Exit status of the last run |
| `rclone_shim_duration_seconds` | Wall-clock duration of the last run |
| `rclone_shim_scrape_success` | `1` if rclone's metrics were sampled at least once |
| `rclone_shim_last_scrape_age_seconds` | Gap between the last sample and the end of the run; `NaN` if none |
| `rclone_shim_info{version,command}` | `1` |

From rclone itself, forwarded unchanged (v1.75): `rclone_bytes_transferred_total`,
`rclone_files_transferred_total`, `rclone_files_deleted_total`, `rclone_dirs_deleted_total`,
`rclone_files_renamed_total`, `rclone_checked_files_total`, `rclone_entries_listed_total`,
`rclone_errors_total`, `rclone_fatal_error`, `rclone_retry_error`, `rclone_speed`, and for
HTTP-based remotes `rclone_http_status_code{host,method,code}`. rclone's Go runtime and
process series are dropped.

### Read this before writing alerts

- **rclone's series are a lower bound.** Its metrics endpoint dies with the process, so the
  shim keeps the last sample it took. Whatever happened after that sample is missing, and
  deletions tend to happen last. `rclone_shim_last_scrape_age_seconds` tells you how much is
  missing. The exit code, duration and timestamps are measured by the shim and are exact.
- **`*_total` series restart at zero every run.** Compare the raw value. `rate()` and
  `increase()` across runs are meaningless.
- **A run with no sample leaves the previous run's `rclone_*` series in place.** A sync that
  fails at once, or finishes in under ~100 ms, has `rclone_shim_scrape_success 0`. Gate
  transfer alerts on `rclone_shim_scrape_success == 1`.
- **A run that is killed never reports.** `SIGKILL`, an OOM kill or a pod deadline leaves the
  old exit code in place. That is what the start timestamp is for: see the first alert below.
- **Pushgateway groups never expire.** When you retire a job, delete its group:
  `curl -X DELETE http://pushgateway:9091/metrics/job/rclone/instance/<name>`.
- **A metric's type must be the same in every group.** If an rclone upgrade changes one, the
  Pushgateway answers `400` until the old groups are deleted. The shim prints the reason.

### Example alerts

```yaml
# Started, and neither finished nor reported within 12 hours: still running, or killed.
- alert: RcloneSyncDidNotFinish
  expr: |
    (
      (rclone_shim_last_start_timestamp_seconds > rclone_shim_last_run_timestamp_seconds)
      or
      (rclone_shim_last_start_timestamp_seconds unless rclone_shim_last_run_timestamp_seconds)
    )
    and (time() - rclone_shim_last_start_timestamp_seconds > 12 * 3600)

- alert: RcloneSyncFailed
  expr: rclone_shim_exit_code != 0
  for: 5m
  annotations:
    summary: "rclone {{ $labels.instance }} exited {{ $value }}"

- alert: RcloneSyncStale
  expr: time() - rclone_shim_last_success_timestamp_seconds > 26 * 3600

# Close to a --max-delete 100 guard (a lower bound, see above).
- alert: RcloneSyncManyDeletes
  expr: rclone_files_deleted_total > 75 and rclone_shim_scrape_success == 1
```

## Kubernetes

The image is `scratch`, so an init container copies the shim into a shared `emptyDir` that the
job container mounts on a directory early in its `PATH`. The job keeps the upstream
`rclone/rclone` image, where `PATH` begins with `/usr/local/sbin` and that directory does not
exist, so nothing is hidden.

```yaml
initContainers:
  - name: shim-install
    image: ghcr.io/sharkusmanch/rclone-metrics-shim:0.1.0
    args: ["install", "/shim/rclone"]
    volumeMounts:
      - { name: shim, mountPath: /shim }
containers:
  - name: sync
    image: rclone/rclone:1.75.1
    command: ["/bin/sh", "-c", "rclone sync /data remote:backup"]
    env:
      - { name: RCLONESHIM_PUSHGATEWAY_URL, value: "http://pushgateway.monitoring.svc:9091" }
      - { name: RCLONESHIM_INSTANCE, value: "data-backup" }
    volumeMounts:
      - { name: shim, mountPath: /usr/local/sbin }
volumes:
  - { name: shim, emptyDir: {} }
```

It runs as any uid with a read-only root filesystem and no capabilities. It needs a writable
temp directory for its socket. rclone treats a metrics address it cannot bind as fatal, so the
shim first proves it can create a socket there; if it cannot, the run proceeds without one and
only rclone's own series are missing. One case the probe cannot see: a real rclone confined to
a private `/tmp` (a snap, for instance). Point `TMPDIR` somewhere both can reach, or set
`RCLONE_METRICS_ADDR` yourself. If the pod's egress is restricted, allow TCP to the Pushgateway.

When the container's command is a shell script, the shell is PID 1 and receives the pod's
`SIGTERM`; it does not pass it on, so rclone and the shim are killed at the end of the grace
period without reporting. Use `exec rclone …` as the script's last command if you want the
signal to reach rclone.

## Plain cron

```sh
PATH=/opt/rclone-shim:/usr/bin:/bin
RCLONESHIM_PUSHGATEWAY_URL=http://pushgateway.lan:9091
RCLONESHIM_INSTANCE=nas-offsite
0 3 * * * rclone sync /srv/data remote:backup
```

## How subcommands are recognised

The shim scans the arguments for the rclone command without knowing rclone's full flag table.
Flags are skipped; a flag that may take a value (`--config /x`) has its value skipped too;
single-dash shorthands and a short list of common long flags are known to be boolean.

The one imprecision: a long boolean flag the shim does not know, placed *before* the command,
makes the command look like that flag's value. `rclone --some-new-bool lsf sync` is then
treated as a `sync`, and its result overwrites that instance's exit code and timestamps. Put
global flags after the command, or use the `--flag=value` form, and it cannot happen.

## Development

```sh
go test ./...                                             # unit + integration (fake rclone)
RCLONE_BIN=$(which rclone) go test -tags e2e ./internal/shim -run TestE2E   # against a real rclone
```

Standard library only. The design and the review that shaped it are in
[docs/plans](docs/plans/2026-10-01-rclone-metrics-shim.md).

## License

MIT
