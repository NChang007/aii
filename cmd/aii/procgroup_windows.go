//go:build windows

package main

import "os/exec"

// setProcessGroup is a no-op on Windows; cancellation kills the shell only.
func setProcessGroup(cmd *exec.Cmd) {}
