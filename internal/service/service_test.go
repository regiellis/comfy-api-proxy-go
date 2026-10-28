package service

import (
	"os"
	"os/exec"
	"testing"
)

func TestIsOursMatchesTheExecutable(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !isOurs(&Record{PID: os.Getpid(), Exe: self}) {
		t.Fatal("this process, running the recorded binary, is not ours")
	}

	// A live process running some other binary must not count, even when
	// its command line mentions the proxy by name.
	cmd := exec.Command("sleep", "30")
	cmd.Args = append(cmd.Args[:1], "30", "comfy-api-proxy")
	if err := cmd.Start(); err != nil {
		t.Skip("sleep unavailable:", err)
	}
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil {
			t.Error(err)
		}
		if err := cmd.Wait(); err == nil {
			t.Error("killed sleep exited cleanly")
		}
	})
	if isOurs(&Record{PID: cmd.Process.Pid, Exe: self}) {
		t.Fatal("a different binary was taken for the proxy")
	}
	if isOurs(&Record{PID: 0x7ffffff0, Exe: self}) {
		t.Fatal("a dead pid was taken for the proxy")
	}
}
