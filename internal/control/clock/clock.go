// Package clock 提供可注入的时钟。控制面所有业务时间（到期、保留期、冷却、滚动窗口、未付款保留）
// 都取自注入的 Clock，而不是数据库的 now()，测试因此可以拨动时间。
package clock

import (
	"sync"
	"time"
)

// Clock 返回当前时间（UTC，精度截到微秒，与 PostgreSQL timestamptz 一致）。
type Clock interface {
	Now() time.Time
}

// System 是真实时钟。
type System struct{}

// Now 实现 Clock。
func (System) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// Fake 是测试用的手动时钟，并发安全。
type Fake struct {
	mu sync.Mutex
	t  time.Time
}

// NewFake 返回停在 t 的时钟。
func NewFake(t time.Time) *Fake { return &Fake{t: t.UTC().Truncate(time.Microsecond)} }

// Now 实现 Clock。
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

// Advance 把时钟向前拨 d。
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d).Truncate(time.Microsecond)
}

// Set 把时钟设为 t。
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = t.UTC().Truncate(time.Microsecond)
}
