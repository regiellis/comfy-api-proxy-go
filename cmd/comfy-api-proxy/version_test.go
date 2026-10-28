package main

import (
	"runtime/debug"
	"testing"
)

func TestBuildVersion(t *testing.T) {
	installed := &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}
	cases := []struct {
		name   string
		linked string
		info   *debug.BuildInfo
		want   string
	}{
		{"release build stamps it", "0.1.0", installed, "0.1.0"},
		{"go install reads the module version", "dev", installed, "0.1.0"},
		{"source build without a stamp", "dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "dev"},
		{"no build info", "dev", nil, "dev"},
	}
	for _, c := range cases {
		if got := buildVersion(c.linked, c.info); got != c.want {
			t.Errorf("%s: buildVersion(%q) = %q, want %q", c.name, c.linked, got, c.want)
		}
	}
}
