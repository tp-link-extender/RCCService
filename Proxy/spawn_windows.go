//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

// consoleSpawn launches the instance in its own real console window (a manual
// start does the same), instead of Go's default redirected file handles. The
// process is still our child, so the restart loop keeps working
func consoleSpawn(i *rccInstance) *exec.Cmd {
	cmd := exec.Command(exePath, "-Console", strconv.Itoa(i.port))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_CONSOLE,
	}
	return cmd
}
