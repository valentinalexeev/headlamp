package agent

import "syscall"

func errReset() error { return syscall.ECONNRESET }
