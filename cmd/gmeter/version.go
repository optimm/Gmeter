package main

import (
	"fmt"
	"io"
	"runtime/debug"
)

// version is stamped by the release build (-ldflags "-X main.version=...").
// Empty means a build that was not stamped, such as go install or go build.
var version = ""

// printVersion implements `gmeter -version`. Without a stamp it falls back to
// the module version Go recorded: a tag for `go install ...@v0.1.0`, or
// "(devel)" for a build from a checkout.
func printVersion(stdout io.Writer) int {
	v := version
	if v == "" {
		v = "(devel)"
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
			v = info.Main.Version
		}
	}
	fmt.Fprintf(stdout, "gmeter %s\n", v)
	return 0
}
