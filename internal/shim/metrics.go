package shim

import (
	"bytes"
	"fmt"
	"strings"
	"time"
)

const rclonePrefix = "rclone_"

// metricName returns the metric name a text-format line refers to, or "".
func metricName(line string) string {
	if strings.HasPrefix(line, "#") {
		f := strings.Fields(line)
		if len(f) >= 3 && (f[1] == "HELP" || f[1] == "TYPE") {
			return f[2]
		}
		return ""
	}
	if i := strings.IndexAny(line, "{ \t"); i >= 0 {
		return line[:i]
	}
	return line
}

// FilterRclone keeps only rclone's own series (and their HELP/TYPE lines) from
// a scrape of rclone's metrics endpoint, dropping Go runtime and process series.
func FilterRclone(scrape []byte) []byte {
	var out bytes.Buffer
	for _, line := range strings.Split(string(scrape), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(metricName(line), rclonePrefix) {
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// Result describes one wrapped rclone run.
type Result struct {
	Command    string
	ExitCode   int
	Start, End time.Time
	Scraped    bool
	LastScrape time.Time
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func seconds(t time.Time) string {
	return fmt.Sprintf("%.3f", float64(t.UnixMilli())/1000)
}

// RenderStart renders the single series pushed when a run begins. A group
// whose start is newer than its last run is still running, or was killed.
func RenderStart(start time.Time) []byte {
	const name = "rclone_shim_last_start_timestamp_seconds"
	return []byte(fmt.Sprintf("# HELP %s Unix time the last wrapped rclone run started.\n# TYPE %s gauge\n%s %s\n", name, name, name, seconds(start)))
}

// RenderShim renders the shim's own series in the Prometheus text format.
func RenderShim(r Result) []byte {
	var b bytes.Buffer
	gauge := func(name, help, value string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", name, help, name, name, value)
	}
	gauge("rclone_shim_last_start_timestamp_seconds", "Unix time the last wrapped rclone run started.", seconds(r.Start))
	gauge("rclone_shim_exit_code", "Exit status of the last wrapped rclone run.", fmt.Sprint(r.ExitCode))
	gauge("rclone_shim_duration_seconds", "Wall-clock duration of the last wrapped rclone run.",
		fmt.Sprintf("%.3f", r.End.Sub(r.Start).Seconds()))
	gauge("rclone_shim_last_run_timestamp_seconds", "Unix time the last wrapped rclone run ended.", seconds(r.End))
	if r.ExitCode == 0 {
		gauge("rclone_shim_last_success_timestamp_seconds", "Unix time the last successful wrapped rclone run ended.", seconds(r.End))
	}
	scraped := "0"
	if r.Scraped {
		scraped = "1"
	}
	gauge("rclone_shim_scrape_success", "1 if rclone's metrics were scraped at least once during the last run.", scraped)
	// Always emitted: with POST semantics an omitted series keeps its old value.
	age := "NaN"
	if r.Scraped {
		age = fmt.Sprintf("%.3f", r.End.Sub(r.LastScrape).Seconds())
	}
	gauge("rclone_shim_last_scrape_age_seconds", "Seconds between the last scrape and the end of the run; rclone_* series lag by this much. NaN when nothing was scraped.", age)
	fmt.Fprintf(&b, "# HELP rclone_shim_info Shim version and the wrapped rclone command.\n# TYPE rclone_shim_info gauge\nrclone_shim_info{version=\"%s\",command=\"%s\"} 1\n",
		labelEscaper.Replace(Version), labelEscaper.Replace(r.Command))
	return b.Bytes()
}
