package node

import (
	"context"
	"time"

	"github.com/go-gost/gost/internal/agent"
	"github.com/go-gost/gost/internal/agentproto"
)

// probeState 跟踪期望状态中的探测任务：每个探测 ID 只执行一次，结果在回传成功前保留并随上报重试。
// 回传按探测 ID 幂等（控制面忽略已完成探测的重复结果），所以重试与重启后重做都安全。
type probeState struct {
	running map[string]bool
	pending map[string]agentproto.ProbeResult // 已完成、尚未回传成功
	sent    map[string]bool                   // 已回传成功；探测从期望状态消失后清除
	sem     chan struct{}
}

func newProbeState() probeState {
	return probeState{
		running: map[string]bool{},
		pending: map[string]agentproto.ProbeResult{},
		sent:    map[string]bool{},
	}
}

// scheduleProbes 启动期望状态中尚未执行的探测。探测直接经 Exit 访问回显端点，不经过客户接入、
// 不进连接表、不计入任何订阅（R-05.5、A-15）。
func (r *Runtime) scheduleProbes(probes []agentproto.Probe) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ps := &r.probes
	if ps.sem == nil {
		ps.sem = make(chan struct{}, r.cfg.Probe.Concurrency)
	}
	listed := make(map[string]bool, len(probes))
	for _, p := range probes {
		listed[p.ID] = true
		if ps.running[p.ID] || ps.sent[p.ID] {
			continue
		}
		if _, done := ps.pending[p.ID]; done {
			continue
		}
		ps.running[p.ID] = true
		r.wg.Add(1)
		go r.runProbe(p)
	}
	for id := range ps.sent {
		if !listed[id] {
			delete(ps.sent, id)
		}
	}
}

func (r *Runtime) runProbe(p agentproto.Probe) {
	defer r.wg.Done()
	select {
	case r.probes.sem <- struct{}{}:
	case <-r.ctx.Done():
		return
	}
	defer func() { <-r.probes.sem }()

	echo := p.EchoURL
	if r.cfg.Probe.EchoURL != "" {
		echo = r.cfg.Probe.EchoURL
	}
	deadline := time.Now().Add(r.cfg.Probe.Timeout)
	if !p.Deadline.IsZero() && p.Deadline.Before(deadline) {
		deadline = p.Deadline
	}
	res := agentproto.ProbeResult{ProbeID: p.ID}
	if time.Until(deadline) <= 0 {
		res.Error = "deadline_exceeded"
	} else {
		ctx, cancel := context.WithDeadline(r.ctx, deadline)
		pr, err := agent.ProbeExit(ctx, toAgentExit(p.Exit, r.cfg.UpstreamTLS), echo)
		cancel()
		if r.ctx.Err() != nil {
			r.mu.Lock()
			delete(r.probes.running, p.ID)
			r.mu.Unlock()
			return // 正在关闭：不回传半截结果，重启后重做
		}
		res.OK = err == nil && pr.Class == agent.ReasonOK
		res.ObservedIP = pr.IP
		res.LatencyMs = pr.Latency.Milliseconds()
		if !res.OK {
			res.Error = string(pr.Class)
			if res.Error == "" {
				res.Error = string(agent.ReasonBadResponse)
			}
			r.log.Warn("出口检测失败", "probe", p.ID, "kind", p.Kind, "subscription", p.SubscriptionID,
				"upstream", p.Exit.Addr, "class", res.Error, "err", err)
		}
	}
	res.CompletedAt = time.Now().UTC()
	r.mu.Lock()
	delete(r.probes.running, p.ID)
	r.probes.pending[p.ID] = res
	r.mu.Unlock()
	r.triggerReport()
}

// postProbeResults 回传尚未回传成功的探测结果。
func (r *Runtime) postProbeResults(ctx context.Context) error {
	r.mu.Lock()
	if len(r.probes.pending) == 0 {
		r.mu.Unlock()
		return nil
	}
	batch := make([]agentproto.ProbeResult, 0, len(r.probes.pending))
	for _, res := range r.probes.pending {
		batch = append(batch, res)
	}
	r.mu.Unlock()
	if err := r.client.PostProbeResults(ctx, batch); err != nil {
		return err
	}
	r.mu.Lock()
	for _, res := range batch {
		delete(r.probes.pending, res.ProbeID)
		r.probes.sent[res.ProbeID] = true
	}
	r.mu.Unlock()
	return nil
}
