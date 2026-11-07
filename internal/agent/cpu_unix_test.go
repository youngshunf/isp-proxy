//go:build unix

package agent

import (
	"syscall"
	"time"
)

// processCPU 返回本进程累计的用户态加内核态 CPU 时间。
func processCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
