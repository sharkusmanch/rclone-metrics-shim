package shim

import "strings"

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

// isBoolean reports whether a flag argument certainly takes no value. Every
// rclone single-dash shorthand is boolean except -f (--filter).
func isBoolean(flag string) bool {
	if strings.Contains(flag, "=") {
		return true // value attached
	}
	if strings.HasPrefix(flag, "--") {
		return longBooleans[flag]
	}
	return !strings.Contains(flag, "f")
}

// noPushFlag reports whether an argument makes this run something whose
// result must not be recorded: a dry run, an interactive run, or a help page.
func noPushFlag(a string) bool {
	switch a {
	case "--dry-run", "--dry-run=true", "--interactive", "--interactive=true", "--help":
		return true
	}
	if len(a) < 2 || a[0] != '-' || a[1] == '-' || strings.Contains(a, "=") {
		return false
	}
	// A cluster of boolean shorthands such as -vn or -nP.
	for _, c := range a[1:] {
		if !strings.ContainsRune("vqPnihcILlMux", c) {
			return false
		}
	}
	return strings.ContainsAny(a[1:], "nih")
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "y", "yes":
		return true
	}
	return false
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
	for _, a := range args {
		if a == "--" {
			break
		}
		if noPushFlag(a) {
			return "", false, false
		}
	}
	for _, name := range []string{"RCLONE_DRY_RUN", "RCLONE_INTERACTIVE"} {
		if v, ok := lookupEnv(env, name); ok && truthy(v) {
			return "", false, false
		}
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
