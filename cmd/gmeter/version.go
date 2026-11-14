package main

import (
	"fmt"
	"io"
	"runtime/debug"
)

// version is stamped by the release build (-ldflags "-X main.version=...").
// Empty means a build that was not stamped, such as go install or go build.
var version = ""

// versionString is the version this binary reports. Without a stamp it falls
// back to the module version Go recorded: a tag for `go install ...@v0.1.0`, or
// "(devel)" for a build from a checkout.
func versionString() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

// printVersion implements `gmeter -version`.
func printVersion(stdout io.Writer) int {
	fmt.Fprintf(stdout, "gmeter %s\n", versionString())
	return 0
}
