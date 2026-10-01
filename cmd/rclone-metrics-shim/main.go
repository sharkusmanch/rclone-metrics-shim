// Command rclone-metrics-shim is a transparent rclone wrapper. Installed under
// the name "rclone" it runs the real rclone and pushes metrics for the run to a
// Prometheus Pushgateway. Under its own name it is a small management CLI.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sharkusmanch/rclone-metrics-shim/internal/shim"
)

const usage = `rclone-metrics-shim: transparent rclone wrapper that pushes metrics to a Pushgateway

Usage:
  rclone-metrics-shim install <dest>   copy this program to <dest> (name it "rclone")
  rclone-metrics-shim version
  rclone-metrics-shim help

When run under the name "rclone" it runs the real rclone found later on PATH.
Configuration is by RCLONESHIM_* environment variables; see the README.
`

func main() {
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	if filepath.Base(os.Args[0]) == "rclone" {
		os.Exit(shim.Run(os.Args[0], os.Args[1:], os.Environ(), self, shim.Stdio{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}))
	}
	os.Exit(manage(os.Args[1:], self))
}

func manage(args []string, self string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version":
		fmt.Println(shim.Version)
		return 0
	case "help", "--help", "-h":
		fmt.Print(usage)
		return 0
	case "install":
		if len(args) != 2 {
			fmt.Fprint(os.Stderr, usage)
			return 2
		}
		if err := shim.Install(self, args[1]); err != nil {
			fmt.Fprintf(os.Stderr, "rclone-metrics-shim: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprint(os.Stderr, usage)
	return 2
}
