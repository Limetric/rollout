package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionStringUsesLinkerValue(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })

	version = "v1.2.3"
	if got := versionString(); got != "v1.2.3" {
		t.Errorf("versionString() = %q, want the value the linker stamped", got)
	}
}

func TestVersionVerboseIncludesBuildMetadata(t *testing.T) {
	out := versionVerboseString()
	for _, want := range []string{"rollout ", "go: ", "platform: "} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose version missing %q:\n%s", want, out)
		}
	}
}

func TestVersionCommandPrintsVersion(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })
	version = "v9.9.9"

	var buf bytes.Buffer
	versionCmd.SetOut(&buf)
	t.Cleanup(func() { versionCmd.SetOut(nil) })
	versionCmd.Run(versionCmd, nil)

	if got := strings.TrimSpace(buf.String()); got != "v9.9.9" {
		t.Errorf("`rollout version` printed %q", got)
	}
}
