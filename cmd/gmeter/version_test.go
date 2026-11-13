package main

import (
	"strings"
	"testing"
)

func TestVersionPrintsTheVersionStampedAtBuild(t *testing.T) {
	old := version
	version = "1.2.3"
	t.Cleanup(func() { version = old })
	for _, arg := range []string{"-version", "--version", "version"} {
		code, stdout, stderr := exec(t, arg)
		if code != 0 || stdout != "gmeter 1.2.3\n" || stderr != "" {
			t.Errorf("%s: code %d, stdout %q, stderr %q; want 0, %q, nothing", arg, code, stdout, stderr, "gmeter 1.2.3\n")
		}
	}
}

func TestVersionWithoutAStampSaysWhatTheBuildKnows(t *testing.T) {
	old := version
	version = ""
	t.Cleanup(func() { version = old })
	code, stdout, _ := exec(t, "-version")
	// a test binary has no module version, so this is "(devel)" or similar; it must not be empty
	if code != 0 || !strings.HasPrefix(stdout, "gmeter ") || strings.TrimSpace(strings.TrimPrefix(stdout, "gmeter ")) == "" {
		t.Errorf("code %d, stdout %q; want 0 and a version after gmeter", code, stdout)
	}
}
