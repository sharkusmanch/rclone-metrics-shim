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
		{"--config /x --transfers 8 -P sync a b", nil, "sync", true, true},
		{"-f /rules sync a b", nil, "sync", true, true},
		{"copyto a b", nil, "copyto", true, true},
		{"moveto a b", nil, "moveto", true, true},
		{"bisync a b", nil, "bisync", true, true},
		// other subcommands
		{"lsf --dirs-only r:", nil, "", false, false},
		{"purge r:x", nil, "", false, false},
		{"purge sync", nil, "", false, false},
		{"lsf --config /x sync", nil, "", false, false},
		{"-v lsf sync", nil, "", false, false},
		{"-v lsf --include copy r:", nil, "", false, false},
		{"-v delete r: --exclude sync", nil, "", false, false},
		{"--fast-list lsf sync", nil, "", false, false},
		{"version", nil, "", false, false},
		{"", nil, "", false, false},
		{"-- sync", nil, "", false, false},
		{"--version", nil, "", false, false},
		// runs whose result must not be recorded
		{"sync a b --dry-run", nil, "", false, false},
		{"sync --dry-run=true a b", nil, "", false, false},
		{"sync -n a b", nil, "", false, false},
		{"sync -vn a b", nil, "", false, false},
		{"sync a b -i", nil, "", false, false},
		{"sync --help", nil, "", false, false},
		{"sync -h", nil, "", false, false},
		{"sync a b", []string{"RCLONE_DRY_RUN=true"}, "", false, false},
		{"sync a b", []string{"RCLONE_DRY_RUN=1"}, "", false, false},
		{"sync a b --dry-run=false", []string{"RCLONE_DRY_RUN=true"}, "sync", true, true},
		{"sync a b -n=false", []string{"RCLONE_DRY_RUN=true"}, "sync", true, true},
		{"bisync a b -1n", nil, "", false, false},
		{"sync a b -nd", nil, "", false, false},
		{"sync a b --dry-run=1", nil, "", false, false},
		{"sync a b -n=true", nil, "", false, false},
		{"sync a b --interactive=true", nil, "", false, false},
		{"sync a b -f -n", nil, "", false, false},
		{"sync a b -fn", nil, "sync", true, true},
		{"sync a b --exclude=-n", nil, "sync", true, true},
		{"sync a b", []string{"RCLONE_INTERACTIVE=true"}, "", false, false},
		{"sync a b", []string{"RCLONE_DRY_RUN=false"}, "sync", true, true},
		{"sync a b --dry-run=false", nil, "sync", true, true},
		{"sync a b -v", nil, "sync", true, true},
		{"sync a b -- -n", nil, "sync", true, true},
		// caller already owns the metrics endpoint
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

// A long boolean flag the shim does not know, placed before the subcommand,
// makes the real subcommand look like that flag's value, so a later argument
// named like a wrapped command is matched. Documented and accepted: the cost is
// one unneeded push. Add the flag to longBooleans rather than teaching the shim
// rclone's whole flag table.
func TestWrapDecision_KnownImprecision(t *testing.T) {
	cfg, _ := LoadConfig([]string{"RCLONESHIM_PUSHGATEWAY_URL=http://p"})
	command, wrap, _ := WrapDecision(strings.Fields("--some-unknown-boolean lsf sync"), cfg, nil)
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
	cfg, _ := LoadConfig([]string{"RCLONESHIM_PUSHGATEWAY_URL=http://p", "RCLONESHIM_COMMANDS=check"})
	if c, wrap, _ := WrapDecision([]string{"check", "a", "b"}, cfg, nil); !wrap || c != "check" {
		t.Fatalf("got (%q,%v)", c, wrap)
	}
	if _, wrap, _ := WrapDecision([]string{"sync", "a", "b"}, cfg, nil); wrap {
		t.Fatal("sync is not in the custom set")
	}
}
