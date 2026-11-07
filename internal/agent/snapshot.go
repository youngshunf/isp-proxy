package agent

import (
	"slices"
	"strings"
	"time"
)

// SubSnapshot 是一个订阅在节点上的观测状态，对应上报中的一条（06 §5）。
type SubSnapshot struct {
	ID            string
	Version       uint64 // 已应用的订阅状态版本（SubState.Version）
	AccessVersion uint64 // 当前接入版本；0 表示没有接入
	ExitVersion   uint64 // 当前出口版本；0 表示没有出口
	UsedTotal     uint64 // 累计已用量，只增不减；控制面以 GREATEST 合并
	Conns         int    // 当前存量连接数（含尚未认证的）
	Usable        bool   // 当前是否允许转发
}

// Snapshot 是节点一次上报的内容。
type Snapshot struct {
	At        time.Time // 本地时钟，供控制面比较时钟偏差
	PersistOK bool      // 本地持久化是否健康；false 时节点已停止授权
	Subs      []SubSnapshot
}

// Snapshot 返回全部订阅的观测状态，按 ID 排序。各字段分别原子读取，
// 不是跨订阅的一致快照；累计用量单调递增，所以重复或乱序上报都可以安全合并。
func (a *Agent) Snapshot() Snapshot {
	a.mu.Lock()
	conns := map[string]int{}
	for svc, id := range a.services {
		conns[id] += len(a.connsLocked(svc))
	}
	snap := Snapshot{At: a.now(), PersistOK: a.persistOK.Load(), Subs: make([]SubSnapshot, 0, len(a.subs))}
	for id, s := range a.subs {
		ss := SubSnapshot{
			ID:        id,
			Version:   s.version.Load(),
			UsedTotal: s.used.Load(),
			Conns:     conns[id],
		}
		if s.access != nil {
			ss.AccessVersion = s.access.Version
		}
		if s.exit != nil {
			ss.ExitVersion = s.exit.current().Version
		}
		snap.Subs = append(snap.Subs, ss)
	}
	a.mu.Unlock()
	for i := range snap.Subs {
		snap.Subs[i].Usable = a.Usable(snap.Subs[i].ID)
	}
	slices.SortFunc(snap.Subs, func(x, y SubSnapshot) int { return strings.Compare(x.ID, y.ID) })
	return snap
}

// PersistOK 报告本地持久化是否健康。
func (a *Agent) PersistOK() bool { return a.persistOK.Load() }

// RevokeAccess 撤销订阅的当前接入与出口（保留期满释放资源）：关闭三个端口、断开全部连接、
// 凭据失效。订阅的累计用量与状态保留，之后可以用 ApplyAccess 重新开通。重复调用无副作用。
func (a *Agent) RevokeAccess(subID string) error {
	a.mu.Lock()
	s := a.subs[subID]
	if s == nil {
		a.mu.Unlock()
		return ErrUnknownSubscription
	}
	old := s.access
	if old != nil {
		delete(a.users, old.Username)
	}
	s.access = nil
	s.exit = nil
	a.mu.Unlock()
	if old != nil {
		a.retire(old)
	}
	return nil
}
