//go:build windows

package service

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

const (
	// detachedProcess gives the child no console, so closing the one that
	// ran start does not end it. syscall does not export it.
	detachedProcess = 0x00000008
	stillActive     = 259
)

// detached starts the child without a console and in its own process
// group, as the reference does on Windows.
func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP}
}

// terminate ends the process at once. A detached process has no console to
// receive Ctrl+Break, so there is no graceful signal to send first; the
// reference uses taskkill /F for the same reason.
func terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		if !alive(pid) {
			return nil
		}
		return fmt.Errorf("open pid %d: %w", pid, err)
	}
	killErr := p.Kill()
	releaseErr := p.Release()
	if killErr != nil && alive(pid) {
		return errors.Join(fmt.Errorf("kill pid %d: %w", pid, killErr), releaseErr)
	}
	return releaseErr
}

// alive asks for the process's exit code; STILL_ACTIVE means it runs. A
// process this user may not open exists, so access denied counts as alive.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	var code uint32
	codeErr := windows.GetExitCodeProcess(h, &code)
	closeErr := windows.CloseHandle(h)
	return codeErr == nil && closeErr == nil && code == stillActive
}

// sameBinary compares the process's image path with want. Unreadable
// counts as a match, as on the other platforms.
func sameBinary(pid int, want string) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return true
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	queryErr := windows.QueryFullProcessImageName(h, 0, &buf[0], &n)
	closeErr := windows.CloseHandle(h)
	if queryErr != nil || closeErr != nil {
		return true
	}
	return strings.EqualFold(windows.UTF16ToString(buf[:n]), want)
}
