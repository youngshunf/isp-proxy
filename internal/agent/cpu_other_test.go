//go:build !unix

package agent

import "time"

// processCPU 在非 Unix 平台不可用，基准测试不报告 cpu-ns/op。
func processCPU() time.Duration { return 0 }
