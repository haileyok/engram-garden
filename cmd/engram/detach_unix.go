//go:build unix

package main

import "syscall"

// detached starts a process in its own session, so it outlives the command.
func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
