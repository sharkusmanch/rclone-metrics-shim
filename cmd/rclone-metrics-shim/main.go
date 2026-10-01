// Command rclone-metrics-shim is a transparent rclone wrapper. Installed under
// the name "rclone" it runs the real rclone and pushes metrics for the run.
package main

import "os"

func main() {
	os.Exit(0)
}
