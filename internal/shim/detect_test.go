package shim

import (
	"strings"
	"testing"
)

func TestWrapDecision(t *testing.T) {
	cfg, _ := LoadConfig([]string{"RCLONESHIM_PUSHGATEWAY_URL=http://p"})
	cases := []struct {
		args        string
		env         []string
		command     string
		wrap, scrap bool
	}{
		{"sync a b", nil, "sync", true, true},
		{"--config /x sync a b", nil, "sync", true, true},
		{"--config=/x -v copy a b", nil, "copy", true, true},
		{"-v --transfers 8 move a b", nil, "move", true, true},
		{"lsf --dirs-only r:", nil, "", false, false},
		{"purge r:x", nil, "", false, false},
		{"purge sync", nil, "", false, false},
		{"lsf --config /x sync", nil, "", false, false},
		{"version", nil, "", false, false},
		{"", nil, "", false, false},
		{"-- sync", nil, "", false, false},
		{"--version", nil, "", false, false},
		{"sync a b --metrics-addr :9", nil, "sync", true, false},
		{"sync a b --metrics-addr=:9", nil, "sync", true, false},
		{"sync a b", []string{"RCLONE_METRICS_ADDR=:9"}, "sync", true, false},
		{"sync a b", []string{"RCLONE_METRICS_ADDR="}, "sync", true, false},
	}
	for _, c := range cases {
		command, wrap, scrape := WrapDecision(strings.Fields(c.args), cfg, c.env)
		if command != c.command || wrap != c.wrap || scrape != c.scrap {
			t.Errorf("%q env=%v: got (%q,%v,%v) want (%q,%v,%v)", c.args, c.env, command, wrap, scrape, c.command, c.wrap, c.scrap)
		}
	}
}

// A boolean flag before the subcommand makes the real subcommand look like a
// flag value, so a later path named like a wrapped command is matched. This is
// documented and accepted: the cost is one unneeded push. Do not "fix" it by
// teaching the shim rclone's flag table.
func TestWrapDecision_KnownImprecision(t *testing.T) {
	cfg, _ := LoadConfig([]string{"RCLONESHIM_PUSHGATEWAY_URL=http://p"})
	command, wrap, _ := WrapDecision(strings.Fields("-v lsf sync"), cfg, nil)
	if command != "sync" || !wrap {
		t.Fatalf("got (%q,%v)", command, wrap)
	}
}

func TestWrapDecision_NoURLIsPassthrough(t *testing.T) {
	cfg, _ := LoadConfig(nil)
	if _, wrap, _ := WrapDecision([]string{"sync", "a", "b"}, cfg, nil); wrap {
		t.Fatal("must not wrap without a pushgateway URL")
	}
}

func TestWrapDecision_CustomCommands(t *testing.T) {
	cfg, _ := LoadConfig([]string{"RCLONESHIM_PUSHGATEWAY_URL=http://p", "RCLONESHIM_COMMANDS=copyto"})
	if c, wrap, _ := WrapDecision([]string{"copyto", "a", "b"}, cfg, nil); !wrap || c != "copyto" {
		t.Fatalf("got (%q,%v)", c, wrap)
	}
	if _, wrap, _ := WrapDecision([]string{"sync", "a", "b"}, cfg, nil); wrap {
		t.Fatal("sync is not in the custom set")
	}
}
