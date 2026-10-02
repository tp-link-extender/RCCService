//go:build !windows

package main

import (
	"os/exec"
	"strconv"
)

// non-Windows fallback: wine launches are already environment-separated
func consoleSpawn(i *rccInstance) *exec.Cmd {
	return exec.Command("wine", exePath, "-Console", strconv.Itoa(i.port))
}
