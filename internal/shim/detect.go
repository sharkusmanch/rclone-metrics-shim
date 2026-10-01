package shim

import (
	"strconv"
	"strings"
)

// longBooleans are common rclone long flags that take no value. A non-flag
// argument after one of these is the subcommand, not a flag value. The list
// need not be complete: an unknown boolean only means the next argument is
// treated as a possible value and scanning continues.
var longBooleans = map[string]bool{
	"--verbose": true, "--quiet": true, "--progress": true, "--dry-run": true,
	"--interactive": true, "--fast-list": true, "--checksum": true, "--update": true,
	"--copy-links": true, "--links": true, "--metadata": true, "--use-json-log": true,
	"--ignore-times": true, "--ignore-existing": true, "--size-only": true,
	"--no-traverse": true, "--one-file-system": true, "--track-renames": true,
	"--delete-excluded": true, "--create-empty-src-dirs": true, "--stats-one-line": true,
	"--help": true, "--version": true,
}

// valueShorthands are the single-dash shorthands that take a value; whatever
// follows one in a cluster (or the next argument) is that value. All other
// rclone shorthands are boolean.
const valueShorthands = "fFst"

// isBoolean reports whether a flag argument certainly takes no value.
func isBoolean(flag string) bool {
	if strings.Contains(flag, "=") {
		return true // value attached
	}
	if strings.HasPrefix(flag, "--") {
		return longBooleans[flag]
	}
	return !strings.ContainsAny(flag[1:], valueShorthands)
}

// parseBool reads a flag value the way rclone (pflag) does; an unparseable
// value counts as true, which errs on the side of not recording the run.
func parseBool(v string) bool {
	b, err := strconv.ParseBool(v)
	return err != nil || b
}

// unrecorded reports whether this invocation is a dry run, an interactive run
// or a help page: runs whose result must never be pushed. An explicit flag
// overrides the RCLONE_DRY_RUN / RCLONE_INTERACTIVE environment variables.
func unrecorded(args, env []string) bool {
	flags := map[byte]bool{}
	for name, key := range map[string]byte{"RCLONE_DRY_RUN": 'n', "RCLONE_INTERACTIVE": 'i'} {
		if v, ok := lookupEnv(env, name); ok && v != "" {
			flags[key] = parseBool(v)
		}
	}
	for _, a := range args {
		if a == "--" {
			break
		}
		if len(a) < 2 || a[0] != '-' {
			continue
		}
		name, value, hasValue := strings.Cut(a, "=")
		on := !hasValue || parseBool(value)
		switch name {
		case "--dry-run":
			flags['n'] = on
		case "--interactive":
			flags['i'] = on
		case "--help":
			flags['h'] = on
		}
		if strings.HasPrefix(name, "--") {
			continue
		}
		// A shorthand cluster such as -vn or -1n; =value belongs to its last flag.
		for i := 1; i < len(name); i++ {
			c := name[i]
			if strings.IndexByte(valueShorthands, c) >= 0 {
				break // the rest of the cluster is this flag's value
			}
			if c == 'n' || c == 'i' || c == 'h' {
				flags[c] = i < len(name)-1 || on
			}
		}
	}
	return flags['n'] || flags['i'] || flags['h']
}

// WrapDecision decides what to do with an rclone invocation.
//
// wrap is true when a Pushgateway is configured, the subcommand is one of
// cfg.Commands and the run is not a dry run, interactive run or help page.
// scrape is false when the caller already configured a metrics endpoint (the
// shim must not replace it); only the shim's own series are pushed then.
//
// The subcommand is found by scanning left to right. Flags are skipped. A
// non-flag argument in cfg.Commands wins. A non-flag argument that follows a
// flag which may take a value is assumed to be that value and scanning
// continues. Any other non-flag argument is a different subcommand.
func WrapDecision(args []string, cfg Config, env []string) (command string, wrap, scrape bool) {
	if cfg.PushgatewayURL == "" {
		return "", false, false
	}
	mayBeValue := false
	for _, a := range args {
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			mayBeValue = !isBoolean(a)
			continue
		}
		if !mayBeValue {
			if cfg.Commands[a] {
				command = a
			}
			break
		}
		if cfg.Commands[a] {
			command = a
			break
		}
		mayBeValue = false
	}
	if command == "" {
		return "", false, false
	}
	if unrecorded(args, env) {
		return "", false, false
	}
	scrape = true
	if _, set := lookupEnv(env, "RCLONE_METRICS_ADDR"); set {
		scrape = false
	}
	for _, a := range args {
		if a == "--metrics-addr" || strings.HasPrefix(a, "--metrics-addr=") {
			scrape = false
		}
	}
	return command, true, scrape
}
