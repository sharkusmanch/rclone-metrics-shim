// Package shim implements a transparent wrapper around rclone that pushes
// Prometheus metrics for a run to a Pushgateway.
package shim

import (
	"fmt"
	"strings"
	"time"
)

// Version is set at build time with -ldflags.
var Version = "dev"

const (
	envPrefix          = "RCLONESHIM_"
	defaultJob         = "rclone"
	defaultCommands    = "sync,copy,move,copyto,moveto,bisync"
	defaultInterval    = time.Second
	minInterval        = 100 * time.Millisecond
	defaultPushTimeout = 10 * time.Second
)

// Config is the shim's configuration, read from the environment only.
type Config struct {
	PushgatewayURL string // "" means pure passthrough
	Job            string
	Instance       string
	Commands       map[string]bool
	Interval       time.Duration
	PushTimeout    time.Duration
	Real           string // absolute path of the real rclone, "" = search PATH
}

// lookupEnv returns the last value of key in an os.Environ-style slice.
func lookupEnv(env []string, key string) (string, bool) {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return env[i][len(prefix):], true
		}
	}
	return "", false
}

func parseCommands(s string) map[string]bool {
	out := map[string]bool{}
	for _, c := range strings.Split(s, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out[c] = true
		}
	}
	return out
}

// LoadConfig never fails: a bad value falls back to its default and adds a warning.
func LoadConfig(env []string) (Config, []string) {
	var warnings []string
	get := func(name string) string {
		v, _ := lookupEnv(env, envPrefix+name)
		return strings.TrimSpace(v)
	}
	duration := func(name string, def, min time.Duration) time.Duration {
		v := get(name)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			warnings = append(warnings, fmt.Sprintf("%s%s=%q is not a valid duration, using %s", envPrefix, name, v, def))
			return def
		}
		if d < min {
			warnings = append(warnings, fmt.Sprintf("%s%s=%s is below the minimum, using %s", envPrefix, name, v, min))
			return min
		}
		return d
	}

	cfg := Config{
		PushgatewayURL: strings.TrimRight(get("PUSHGATEWAY_URL"), "/"),
		Job:            get("JOB"),
		Instance:       get("INSTANCE"),
		Real:           get("REAL"),
	}
	if cfg.Job == "" {
		cfg.Job = defaultJob
	}
	cfg.Commands = parseCommands(get("COMMANDS"))
	if len(cfg.Commands) == 0 {
		cfg.Commands = parseCommands(defaultCommands)
	}
	cfg.Interval = duration("INTERVAL", defaultInterval, minInterval)
	cfg.PushTimeout = duration("PUSH_TIMEOUT", defaultPushTimeout, time.Millisecond)
	return cfg, warnings
}
