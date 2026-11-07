package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// offsetClock 是开发与平台 E2E 用的可拨动时钟（dev.clock_offset_file）：业务时间 = 系统时间 + 文件中的偏移。
// 只影响控制面（到期判定、保留期、未付款预留、冷却、滚动窗口、会话期限），不影响节点 agent 的本地计时器。
type offsetClock struct {
	path   string
	offset atomic.Int64 // 纳秒
	log    *slog.Logger
}

func newOffsetClock(path string, log *slog.Logger) *offsetClock {
	c := &offsetClock{path: path, log: log}
	c.reload()
	return c
}

// Now 返回带偏移的当前时间。
func (c *offsetClock) Now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

func (c *offsetClock) reload() {
	b, err := os.ReadFile(c.path)
	var d time.Duration
	if err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			v, perr := time.ParseDuration(s)
			if perr != nil {
				c.log.Warn("dev.clock_offset_file 内容不是 duration，忽略", "content", s)
				return
			}
			d = v
		}
	}
	if old := time.Duration(c.offset.Swap(int64(d))); old != d {
		c.log.Warn("开发时钟偏移已变化", "offset", d.String())
	}
}

// run 每 200 毫秒重新读取偏移文件。
func (c *offsetClock) run(ctx context.Context) {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.reload()
		}
	}
}
