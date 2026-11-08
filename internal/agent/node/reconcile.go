package node

import (
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-gost/gost/internal/agent"
	"github.com/go-gost/gost/internal/agentproto"
)

// errCertUnavailable 表示 HTTPS 入口证书不可用，无法开通接入。
var errCertUnavailable = errors.New("agent: HTTPS 入口证书不可用")

// reconcile 把一份期望状态应用到数据面（声明式对账）。source 只用于日志（control / local）。
//
// 规则（docs/06 §5、§8.3）：
//   - 只应用版本比已应用版本更高的条目；版本相同或更低的条目跳过（控制面从备份恢复导致回退时也不回滚）。
//   - 应用失败的版本记为 ApplyError 上报，已应用版本不变；下一份期望状态到来时重试。
//   - 列表中消失的订阅撤销接入并断开连接。
func (r *Runtime) reconcile(d *agentproto.Desired, source string) {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	if err := d.Validate(); err != nil {
		r.log.Error("期望状态未通过校验，整份忽略", "source", source, "revision", d.Revision, "err", err)
		return
	}

	want := make(map[string]bool, len(d.Subscriptions))
	for _, e := range d.Subscriptions {
		want[e.ID] = true
		r.mu.Lock()
		cur, have := r.applied[e.ID]
		if have && e.Version <= cur.Version {
			delete(r.errs, e.ID) // 失败的更高版本已不再是期望状态
			r.mu.Unlock()
			continue
		}
		r.mu.Unlock()
		var curp *agentproto.SubscriptionEntry
		if have {
			curp = &cur
		}
		err := r.applyEntry(curp, e)
		r.mu.Lock()
		delete(r.retired, e.ID)
		if err != nil {
			ae := &agentproto.ApplyError{Version: e.Version, Code: applyCode(err), Message: err.Error()}
			r.errs[e.ID] = ae
			r.mu.Unlock()
			r.log.Error("应用订阅失败", "source", source, "subscription", e.ID, "version", e.Version, "code", ae.Code, "err", err)
			continue
		}
		r.applied[e.ID] = e
		delete(r.errs, e.ID)
		r.dirty = true
		r.mu.Unlock()
		r.log.Info("已应用订阅", "source", source, "subscription", e.ID, "version", e.Version,
			"access_version", accessVersion(e.Access), "exit_version", exitVersion(e.Exit))
	}

	// 列表中消失的订阅：撤销接入（关闭端口、断开连接、凭据失效），最终用量上报确认后再从节点删除。
	r.mu.Lock()
	var gone []string
	for id := range r.applied {
		if !want[id] {
			gone = append(gone, id)
		}
	}
	for id := range r.errs {
		if !want[id] {
			delete(r.errs, id)
			if _, ok := r.applied[id]; !ok {
				r.retired[id] = true
			}
		}
	}
	r.mu.Unlock()
	for _, id := range gone {
		if err := r.agent.RevokeAccess(id); err != nil && !errors.Is(err, agent.ErrUnknownSubscription) {
			r.log.Error("撤销订阅失败", "subscription", id, "err", err)
		}
		r.mu.Lock()
		delete(r.applied, id)
		r.retired[id] = true
		r.dirty = true
		r.mu.Unlock()
		r.log.Info("已撤销订阅", "source", source, "subscription", id)
	}

	r.mu.Lock()
	if r.revision != d.Revision {
		r.revision = d.Revision
		r.dirty = true
	}
	r.mu.Unlock()
	if source == "control" {
		r.persist()
		r.scheduleProbes(d.Probes)
	}
	r.triggerReport()
}

// applyEntry 把订阅从 cur（可能为空：从未应用）变为 e。可能失败的步骤在前，失败时数据面保持 cur 的状态；
// 订阅状态（额度、期限、启用、上限）在接入与出口成功之后才应用。
func (r *Runtime) applyEntry(cur *agentproto.SubscriptionEntry, e agentproto.SubscriptionEntry) error {
	id := e.ID
	r.agent.Ensure(id)
	var curAcc *agentproto.Access
	var curExit *agentproto.Exit
	if cur != nil {
		curAcc, curExit = cur.Access, cur.Exit
	}
	switch {
	case e.Access == nil:
		// 没有接入：不监听任何端口。出口在没有接入时没有意义。
		if curAcc != nil {
			if err := r.agent.RevokeAccess(id); err != nil {
				return err
			}
		}

	case curAcc == nil || curAcc.Ports != e.Access.Ports:
		// 开通或换接入（含组合更换）：新的三个服务直接绑定期望的出口，就绪后撤销旧接入（docs/04 §4.1、§4.3）。
		if r.certs.Leaf() == nil {
			return errCertUnavailable
		}
		var err error
		if e.Direct {
			// 直出订阅（D-25）：只有显式标记直出的条目经本机拨号；未标记且没有出口的条目照旧失败即拒绝。
			err = r.agent.ReplaceAccessDirect(id, r.access(e.Access))
		} else {
			err = r.agent.ReplaceAccess(id, r.access(e.Access), r.exit(e.Exit))
		}
		if err != nil {
			return err
		}

	default:
		// 端口不变：先校验出口（唯一可能失败的一步），再原地替换凭据、换出口。
		curDirect := cur != nil && cur.Direct
		exitChanged := !exitEqual(curExit, e.Exit) || curDirect != e.Direct
		if exitChanged && e.Exit != nil {
			if err := agent.ValidateExit(*r.exit(e.Exit)); err != nil {
				return err
			}
		}
		if *curAcc != *e.Access {
			// 重置密码（A-41）或同端口的新接入版本：旧凭据立即失效，存量连接关闭。
			if err := r.agent.UpdateCredentials(id, e.Access.Version, e.Access.Username, e.Access.Password); err != nil {
				return err
			}
		}
		if exitChanged {
			// 换出口（docs/04 §4.2）：原子替换链内容并关闭存量连接。
			var err error
			switch {
			case e.Direct:
				err = r.agent.ApplyDirect(id)
			case e.Exit == nil:
				err = r.agent.ClearExit(id)
			default:
				err = r.agent.ApplyExit(id, *r.exit(e.Exit))
			}
			if err != nil {
				return err
			}
		}
	}
	r.agent.Upsert(subState(e))
	return nil
}

// subState 把期望状态条目换成数据面的订阅状态。
func subState(e agentproto.SubscriptionEntry) agent.SubState {
	exp := e.ExpiresAt
	if exp.IsZero() {
		exp = time.Unix(0, 0) // 未起算：不可用
	}
	var bps int64
	if e.BandwidthBps > 0 {
		bps = int64(min((e.BandwidthBps+7)/8, 1<<62)) // 期望状态是 bit/s，数据面按字节/秒
	}
	return agent.SubState{
		ID:            e.ID,
		Version:       e.Version,
		EntitledTotal: e.EntitledTotal,
		ExpiresAt:     exp,
		Enabled:       e.Enabled,
		UsedFloor:     e.UsedFloor,
		MaxConns:      e.MaxConns,
		BandwidthBps:  bps,
		Unmetered:     e.Unmetered,
	}
}

func (r *Runtime) access(a *agentproto.Access) agent.Access {
	h := r.cfg.Entry.BindHost
	addr := func(p int) string { return net.JoinHostPort(h, strconv.Itoa(p)) }
	return agent.Access{
		Version:    a.Version,
		Username:   a.Username,
		Password:   a.Password,
		HTTPAddr:   addr(a.Ports.HTTP),
		HTTPSAddr:  addr(a.Ports.HTTPS),
		SOCKS5Addr: addr(a.Ports.SOCKS5),
	}
}

// exit 换成数据面的出口；nil 表示没有出口（失败即拒绝）。
func (r *Runtime) exit(e *agentproto.Exit) *agent.Exit {
	if e == nil {
		return nil
	}
	x := toAgentExit(*e, r.cfg.UpstreamTLS)
	return &x
}

func toAgentExit(e agentproto.Exit, t UpstreamTLSConfig) agent.Exit {
	return agent.Exit{
		Version:     e.Version,
		Protocol:    e.Protocol,
		Addr:        e.Addr,
		Username:    e.Username,
		Password:    e.Password,
		TLSCAFile:   t.CAFile,
		TLSInsecure: t.InsecureSkipVerify,
	}
}

func exitEqual(a, b *agentproto.Exit) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func accessVersion(a *agentproto.Access) uint64 {
	if a == nil {
		return 0
	}
	return a.Version
}

func exitVersion(e *agentproto.Exit) uint64 {
	if e == nil {
		return 0
	}
	return e.Version
}

// applyCode 把应用错误归类为协议中的失败代码。
func applyCode(err error) string {
	switch {
	case errors.Is(err, syscall.EADDRINUSE), strings.Contains(err.Error(), "address already in use"),
		strings.Contains(err.Error(), "Only one usage of each socket address"):
		return agentproto.ApplyPortInUse
	case errors.Is(err, errCertUnavailable):
		return agentproto.ApplyCertUnavailable
	case errors.Is(err, agent.ErrExitInvalid):
		return agentproto.ApplyExitInvalid
	default:
		return agentproto.ApplyInternal
	}
}

func sortEntries(es []agentproto.SubscriptionEntry) {
	slices.SortFunc(es, func(a, b agentproto.SubscriptionEntry) int { return strings.Compare(a.ID, b.ID) })
}
