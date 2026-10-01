package shim

import (
	"reflect"
	"testing"
	"time"
)

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, warns := LoadConfig([]string{"PATH=/bin"})
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	want := Config{
		Job:         "rclone",
		Commands:    map[string]bool{"sync": true, "copy": true, "move": true},
		Interval:    time.Second,
		PushTimeout: 10 * time.Second,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v\nwant %+v", cfg, want)
	}
}

func TestLoadConfig_Overrides(t *testing.T) {
	cfg, warns := LoadConfig([]string{
		"RCLONESHIM_PUSHGATEWAY_URL=http://pgw:9091/",
		"RCLONESHIM_JOB=backup",
		"RCLONESHIM_INSTANCE=books",
		"RCLONESHIM_COMMANDS= sync , copyto ",
		"RCLONESHIM_INTERVAL=5s",
		"RCLONESHIM_PUSH_TIMEOUT=250ms",
		"RCLONESHIM_REAL=/opt/rclone",
	})
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	want := Config{
		PushgatewayURL: "http://pgw:9091",
		Job:            "backup",
		Instance:       "books",
		Commands:       map[string]bool{"sync": true, "copyto": true},
		Interval:       5 * time.Second,
		PushTimeout:    250 * time.Millisecond,
		Real:           "/opt/rclone",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v\nwant %+v", cfg, want)
	}
}

func TestLoadConfig_BadValuesFallBackWithWarning(t *testing.T) {
	cfg, warns := LoadConfig([]string{"RCLONESHIM_INTERVAL=banana", "RCLONESHIM_PUSH_TIMEOUT=-3s"})
	if cfg.Interval != time.Second || cfg.PushTimeout != 10*time.Second {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if len(warns) != 2 {
		t.Fatalf("want 2 warnings, got %v", warns)
	}
}

func TestLoadConfig_IntervalClamped(t *testing.T) {
	cfg, warns := LoadConfig([]string{"RCLONESHIM_INTERVAL=1ms"})
	if cfg.Interval != 100*time.Millisecond || len(warns) != 1 {
		t.Fatalf("got %s, warnings %v", cfg.Interval, warns)
	}
}

func TestLoadConfig_EmptyCommandsUsesDefault(t *testing.T) {
	cfg, _ := LoadConfig([]string{"RCLONESHIM_COMMANDS= , "})
	if !cfg.Commands["sync"] || len(cfg.Commands) != 3 {
		t.Fatalf("got %v", cfg.Commands)
	}
}

func TestLoadConfig_LastDuplicateWins(t *testing.T) {
	cfg, _ := LoadConfig([]string{"RCLONESHIM_JOB=a", "RCLONESHIM_JOB=b"})
	if cfg.Job != "b" {
		t.Fatalf("got %q", cfg.Job)
	}
}
