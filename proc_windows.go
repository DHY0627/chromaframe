//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

func configureChild(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
