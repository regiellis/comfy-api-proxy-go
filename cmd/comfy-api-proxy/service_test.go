package main

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// binary builds the CLI once per test run. start re-executes os.Executable,
// which inside a test is the test binary, so the lifecycle has to drive a
// real build.
func binary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "comfy-api-proxy")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func portOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	return conn.Close() == nil
}

type result struct {
	code           int
	stdout, stderr string
}

func cli(t *testing.T, bin string, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return result{code, out.String(), errOut.String()}
}

func testEnv(t *testing.T) []string {
	t.Helper()
	env := []string{"COMFY_API_PROXY_STATE_DIR=" + t.TempDir(), "XDG_CONFIG_HOME=" + t.TempDir(), "HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	return env
}

func TestStartStatusStopCycle(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and spawns the binary")
	}
	bin := binary(t)
	env := testEnv(t)
	port := freePort(t)
	start := []string{"start", "--port", strconv.Itoa(port), "--comfyui", "http://127.0.0.1:1"}

	if r := cli(t, bin, env, "status"); r.code != 1 || !strings.Contains(r.stdout, "not running") {
		t.Fatalf("status before start = %+v", r)
	}
	r := cli(t, bin, env, start...)
	t.Cleanup(func() { cli(t, bin, env, "stop") })
	if r.code != 0 || !strings.Contains(r.stdout, "started") {
		t.Fatalf("start = %+v", r)
	}
	if !portOpen(port) {
		t.Fatal("port not open after start")
	}
	if r := cli(t, bin, env, "status"); r.code != 0 || !strings.Contains(r.stdout, "running") || !strings.Contains(r.stdout, "healthy") {
		t.Fatalf("status while running = %+v", r)
	}
	if r := cli(t, bin, env, start...); r.code != 1 || !strings.Contains(r.stderr, "already running") {
		t.Fatalf("second start = %+v", r)
	}

	if r := cli(t, bin, env, "stop"); r.code != 0 || !strings.Contains(r.stdout, "stopped") {
		t.Fatalf("stop = %+v", r)
	}
	for i := 0; i < 50 && portOpen(port); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if portOpen(port) {
		t.Fatal("port still open after stop")
	}
	if r := cli(t, bin, env, "status"); r.code != 1 {
		t.Fatalf("status after stop = %+v", r)
	}
}

func TestStartRefusesTakenPortAndBadBind(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and spawns the binary")
	}
	bin := binary(t)
	env := testEnv(t)

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
	if r := cli(t, bin, env, "start", "--port", port); r.code != 1 || !strings.Contains(r.stderr, "already in use") {
		t.Fatalf("start on taken port = %+v", r)
	}
	if r := cli(t, bin, env, "start", "--host", "global"); r.code != 2 || !strings.Contains(r.stderr, "refusing to bind") {
		t.Fatalf("start global without token = %+v", r)
	}
}

func TestStopWhenNotRunningAndCorruptRecord(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and spawns the binary")
	}
	bin := binary(t)
	stateDir := t.TempDir()
	env := append(testEnv(t), "COMFY_API_PROXY_STATE_DIR="+stateDir)
	if r := cli(t, bin, env, "stop"); r.code != 0 || !strings.Contains(r.stdout, "not running") {
		t.Fatalf("stop with nothing running = %+v", r)
	}
	for _, bad := range []string{"not json at all", "[]", `{"pid": "abc"}`, `{"nope": 1}`, `{"pid": 999999}`} {
		if err := os.WriteFile(filepath.Join(stateDir, "proxy.json"), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if r := cli(t, bin, env, "status"); r.code != 1 || !strings.Contains(r.stdout, "not running") {
			t.Errorf("status with %q = %+v", bad, r)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "proxy.json"), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if r := cli(t, bin, env, "stop"); r.code != 0 {
			t.Errorf("stop with %q = %+v", bad, r)
		}
	}
}

func TestConfigCommandShowsSources(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("COMFY_PROXY_PORT", "9123")
	var out, errOut bytes.Buffer
	if code := run([]string{"config", "--host", "global", "--token", "abc"}, &out, &errOut); code != 0 {
		t.Fatalf("config exit %d: %s", code, errOut.String())
	}
	for _, want := range []string{"port: 9123 (env COMFY_PROXY_PORT)", "host: global → 0.0.0.0 (flag --host)", "token: set (3 characters) (flag --token)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("config output missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "abc") {
		t.Error("config output leaked the token")
	}
}

func TestPassedArgs(t *testing.T) {
	cmd := newStartCmd(&bytes.Buffer{})
	if err := cmd.ParseFlags([]string{"--port", "9000", "--enable-cors-header", "https://a.example", "--enable-cors-header", "https://b.example", "--quiet"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(passedArgs(cmd), " ")
	want := "--enable-cors-header=https://a.example --enable-cors-header=https://b.example --port=9000 --quiet=true"
	if got != want {
		t.Fatalf("passedArgs = %q, want %q", got, want)
	}
}
