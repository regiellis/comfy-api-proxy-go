package main

import (
	"bytes"
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestWithDefaultCommand(t *testing.T) {
	root := newRootCmd(&bytes.Buffer{})
	cases := map[string]string{
		"":                "run",
		"--port 9000":     "run --port 9000",
		"run --port 9000": "run --port 9000",
		"--help":          "--help",
		"help run":        "help run",
		"completion zsh":  "completion zsh",
		"--version":       "--version",
		"man":             "man",
		"status":          "status",
	}
	for in, want := range cases {
		got := strings.Join(withDefaultCommand(root, strings.Fields(in)), " ")
		if got != want {
			t.Errorf("withDefaultCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunExitCodes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, k := range []string{"COMFY_PROXY_HOST", "COMFY_PROXY_PORT", "COMFY_PROXY_TOKEN", "COMFY_PROXY_CONFIG"} {
		t.Setenv(k, "")
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"--host", "global"}, &out, &errOut); code != exitConfig {
		t.Fatalf("global without token: exit %d, want %d (stderr %q)", code, exitConfig, errOut.String())
	}
	if !strings.Contains(errOut.String(), "refusing to bind") {
		t.Fatalf("stderr = %q", errOut.String())
	}

	taken, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := taken.Close(); err != nil {
			t.Error(err)
		}
	})
	port := strconv.Itoa(taken.Addr().(*net.TCPAddr).Port)

	errOut.Reset()
	if code := run([]string{"--port", port}, &out, &errOut); code != exitRuntime {
		t.Fatalf("taken port: exit %d, want %d (stderr %q)", code, exitRuntime, errOut.String())
	}
	for _, want := range []string{"port already in use", "flag --port", "COMFY_PROXY_PORT"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr %q missing %q", errOut.String(), want)
		}
	}
}
