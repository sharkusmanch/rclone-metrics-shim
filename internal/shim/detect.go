package shim

import "strings"

// WrapDecision decides what to do with an rclone invocation.
//
// wrap is true when a Pushgateway is configured and the subcommand is one of
// cfg.Commands. scrape is false when the caller already configured a metrics
// endpoint (the shim must not replace it), in which case only the shim's own
// series are pushed.
//
// The subcommand is found by scanning left to right: flags are skipped; a
// non-flag argument in cfg.Commands wins; a non-flag argument right after a
// flag without "=" may be that flag's value, so scanning continues; any other
// non-flag argument is some other subcommand and means passthrough.
func WrapDecision(args []string, cfg Config, env []string) (command string, wrap, scrape bool) {
	if cfg.PushgatewayURL == "" {
		return "", false, false
	}
	prevIsBareFlag := false
	for _, a := range args {
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			prevIsBareFlag = !strings.Contains(a, "=")
			continue
		}
		if cfg.Commands[a] {
			command = a
			break
		}
		if !prevIsBareFlag {
			break
		}
		prevIsBareFlag = false
	}
	if command == "" {
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
