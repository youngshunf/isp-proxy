// Package node 是节点 agent 的运行时（M3，A-W7～A-W9）：把 internal/agent 的数据面执行层接到控制面。
//
//   - 用 mTLS 连接控制面（agentproto.Client.Watch），对每份期望状态做声明式对账：
//     订阅额度、期限、启用与上限按版本应用；接入（三个服务）按端口变化换接入、按凭据变化原地重置；
//     出口按内容变化换出口；接入与出口同一版本变化时组合更换；列表中消失的订阅撤销接入。
//   - 某个订阅应用失败时，错误写入上报（ApplyError），已应用版本不前进，其他订阅不受影响（A-17）。
//   - 最后一次已应用的期望状态落盘（desired.json，0600），控制面离线时重启也能恢复服务并继续执行到期与额度。
//   - 每 ≤ 5 秒（以及每次对账之后）上报累计用量；控制面不可用时退避重试，上报是累计值，重试安全。
//   - 执行期望状态中的探测（ProbeExit），按探测 ID 回传结果；探测不经客户接入，不计量。
//
// 进程内只能有一个运行时（agent.Install 是进程级的）。
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-gost/gost/internal/agent"
	"github.com/go-gost/gost/internal/agentproto"
)

// Options 是运行时的可选参数。
type Options struct {
	// AgentVersion 写入上报（gost 版本号）。
	AgentVersion string
	// Logger 默认 slog.Default()。日志中不出现接入密码与上游凭据。
	Logger *slog.Logger
	// RetryAfterReject 是控制面拒绝节点（401/403）后重新连接的间隔，默认 30 秒。
	RetryAfterReject time.Duration
}

// Runtime 是运行中的节点 agent。
type Runtime struct {
	cfg    Config
	opts   Options
	log    *slog.Logger
	agent  *agent.Agent
	certs  *agent.CertReloader
	client *agentproto.Client

	stateFile string
	stopCerts func()
	persistMu sync.Mutex // 串行化 desired.json 写入，避免旧快照覆盖新快照

	applyMu sync.Mutex // 串行化对账

	mu       sync.Mutex
	applied  map[string]agentproto.SubscriptionEntry // 已成功应用的条目
	errs     map[string]*agentproto.ApplyError       // 更高版本的应用失败
	retired  map[string]bool                         // 已撤销、最终用量尚未被控制面确认的订阅
	revision uint64                                  // 最近一次对账的修订号
	dirty    bool                                    // desired.json 待写
	skew     *agentproto.ClockSkew
	skewBad  bool
	probes   probeState

	reportNow chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// Start 启动运行时：恢复本地状态（不等控制面），然后连接控制面并开始上报。
func Start(cfg *Config, opts Options) (*Runtime, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RetryAfterReject <= 0 {
		opts.RetryAfterReject = 30 * time.Second
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: 数据目录: %w", err)
	}
	_ = os.Chmod(cfg.DataDir, 0o700)

	tlsCfg, err := agentproto.LoadClientTLSConfig(cfg.Control.CAFile, cfg.Control.CertFile, cfg.Control.KeyFile, cfg.Control.ServerName)
	if err != nil {
		return nil, err
	}
	certs, err := agent.NewCertReloader(cfg.Entry.CertFile, cfg.Entry.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("agent: HTTPS 入口证书: %w", err)
	}
	st, err := loadState(filepath.Join(cfg.DataDir, stateFile))
	if err != nil {
		return nil, fmt.Errorf("agent: 本地期望状态: %w", err)
	}
	a, err := agent.New(agent.Options{
		Store:      agent.FileStore{Path: filepath.Join(cfg.DataDir, usedFile)},
		FlushEvery: cfg.FlushInterval,
		Egress:     agent.EgressPolicy{AllowCIDRs: cfg.allowCIDRs(), AllowPorts: cfg.Egress.AllowPorts},
	})
	if err != nil {
		return nil, fmt.Errorf("agent: 本地用量: %w", err)
	}
	agent.Install(a, certs.TLSConfig())

	r := &Runtime{
		cfg:       *cfg,
		opts:      opts,
		log:       opts.Logger.With("component", "isp-agent"),
		agent:     a,
		certs:     certs,
		client:    agentproto.NewClient(cfg.Control.URL, tlsCfg),
		stateFile: filepath.Join(cfg.DataDir, stateFile),
		applied:   map[string]agentproto.SubscriptionEntry{},
		errs:      map[string]*agentproto.ApplyError{},
		retired:   map[string]bool{},
		probes:    newProbeState(),
		reportNow: make(chan struct{}, 1),
	}
	r.stopCerts = certs.Watch(cfg.Entry.CertReloadInterval)
	r.ctx, r.cancel = context.WithCancel(context.Background())

	r.restore(st)

	r.wg.Add(2)
	go r.watchLoop()
	go r.reportLoop()
	r.log.Info("agent 已启动", "node_id", cfg.NodeID, "control", cfg.Control.URL,
		"restored_subscriptions", len(st.Subscriptions), "restored_revision", st.Revision)
	return r, nil
}

// Agent 返回数据面执行层（测试与诊断用）。
func (r *Runtime) Agent() *agent.Agent { return r.agent }

// Close 停止对账与上报，尽力做最后一次上报，关闭全部接入服务，最后落盘累计用量。可重复调用。
func (r *Runtime) Close() error {
	r.closeOnce.Do(func() {
		r.cancel()
		r.wg.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = r.reportOnce(ctx) // 最后一次上报，失败无妨：累计值会在下次启动后补报
		cancel()
		r.applyMu.Lock()
		r.agent.Shutdown()
		r.applyMu.Unlock()
		r.stopCerts()
		r.closeErr = r.agent.Close()
		r.persist()
	})
	return r.closeErr
}

// restore 在连接控制面之前按本地保存的期望状态恢复（R-14.7、docs/04 §8 局限 6）。
// 本地有用量、但不在保存状态里的订阅视为已撤销：照常上报一次最终用量，确认后删除。
func (r *Runtime) restore(st *savedState) {
	if len(st.Subscriptions) > 0 {
		r.reconcile(&agentproto.Desired{Revision: st.Revision, Subscriptions: st.Subscriptions}, "local")
	}
	r.mu.Lock()
	r.revision = st.Revision
	known := map[string]bool{}
	for id := range r.applied {
		known[id] = true
	}
	for id := range r.errs {
		known[id] = true
	}
	r.mu.Unlock()
	for _, s := range r.agent.Snapshot().Subs {
		if !known[s.ID] {
			r.mu.Lock()
			r.retired[s.ID] = true
			r.mu.Unlock()
		}
	}
}

// watchLoop 跟随控制面的期望状态。网络错误与 5xx 由 Watch 内部退避重试；
// 401/403（证书或节点被停用）时按最后已知状态继续执行，隔一段时间再试。
func (r *Runtime) watchLoop() {
	defer r.wg.Done()
	for {
		err := r.client.Watch(r.ctx, func(d *agentproto.Desired) error {
			if r.cfg.NodeID != "" && d.NodeID != "" && d.NodeID != r.cfg.NodeID {
				r.log.Warn("期望状态的节点 ID 与配置不一致（以证书为准）", "config", r.cfg.NodeID, "desired", d.NodeID)
			}
			r.reconcile(d, "control")
			return nil
		})
		if r.ctx.Err() != nil {
			return
		}
		var se *agentproto.StatusError
		if errors.As(err, &se) {
			r.log.Error("控制面拒绝本节点，按最后已知状态继续执行", "status", se.Status, "code", se.Body.Code, "err", se.Body.Message)
		} else {
			r.log.Error("跟随期望状态失败", "err", err)
		}
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(r.opts.RetryAfterReject):
		}
	}
}

func (r *Runtime) triggerReport() {
	select {
	case r.reportNow <- struct{}{}:
	default:
	}
}

// persist 写 desired.json（脏时）。失败只记录日志并在下次上报周期重试：它不影响计量与执行，
// 只影响“控制面离线时重启”的恢复。
func (r *Runtime) persist() {
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return
	}
	st := &savedState{Format: stateFormat, Revision: r.revision, SavedAt: time.Now().UTC()}
	for _, e := range r.applied {
		st.Subscriptions = append(st.Subscriptions, e)
	}
	r.dirty = false
	r.mu.Unlock()
	sortEntries(st.Subscriptions)
	if err := saveState(r.stateFile, st); err != nil {
		r.log.Error("保存本地期望状态失败", "file", r.stateFile, "err", err)
		r.mu.Lock()
		r.dirty = true
		r.mu.Unlock()
	}
}
