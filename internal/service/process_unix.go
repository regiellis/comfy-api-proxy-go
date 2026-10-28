//go:build !windows

package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// detached puts the child in its own session, so closing the terminal that
// ran start does not send it SIGHUP.
func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// terminate sends SIGTERM, waits for a graceful exit, then SIGKILL.
func terminate(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(stopTimeout)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill pid %d: %w", pid, err)
	}
	return nil
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// sameBinary reports whether pid runs want. On Linux that is
// /proc/<pid>/exe, which reads "<path> (deleted)" once the binary is rebuilt
// under a running proxy. Elsewhere (macOS) it is the first word of the
// command line plus the run subcommand. Unreadable counts as a match.
func sameBinary(pid int, want string) bool {
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return strings.TrimSuffix(exe, " (deleted)") == want
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return true
	}
	fields := strings.Fields(string(out))
	return len(fields) >= 2 && filepath.Base(fields[0]) == filepath.Base(want) && fields[1] == "run"
}
