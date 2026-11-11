//go:build linux

package cmd

import "syscall"

// syscallSetEuid changes the effective user of every thread of the process, keeping root as the saved
// user so that the test can become root again.
func syscallSetEuid(uid int) error { return syscall.Setresuid(-1, uid, 0) }
