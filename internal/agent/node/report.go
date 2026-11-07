package node

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/go-gost/gost/internal/agent"
	"github.com/go-gost/gost/internal/agentproto"
)

const (
	minRetry = 200 * time.Millisecond
)

// reportLoop 每 ReportInterval（≤ 5 秒）以及每次对账之后上报一次。控制面不可用时从 200 ms 起指数退避，
// 最长不超过上报间隔；上报是累计值，重试与重复都安全。探测结果随上报一同回传。
func (r *Runtime) reportLoop() {
	defer r.wg.Done()
	interval := r.cfg.ReportInterval
	backoff := minRetry
	timer := time.NewTimer(0) // 启动后立即上报一次
	defer timer.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-timer.C:
		case <-r.reportNow:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		r.persist() // 重试此前失败的 desired.json 写入
		err := r.reportOnce(r.ctx)
		if err == nil {
			err = r.postProbeResults(r.ctx)
		}
		next := interval
		if err != nil {
			if r.ctx.Err() != nil {
				return
			}
			r.log.Warn("上报控制面失败，稍后重试", "retry_in", backoff, "err", err)
			next = backoff
			backoff = min(backoff*2, interval)
		} else {
			backoff = minRetry
		}
		timer.Reset(next)
	}
}

// buildReport 生成一次上报：已应用、应用失败与已撤销待确认的订阅，按 ID 排序。
func (r *Runtime) buildReport() (*agentproto.Report, []string) {
	r.mu.Lock()
	ids := make(map[string]bool, len(r.applied)+len(r.errs)+len(r.retired))
	for id := range r.applied {
		ids[id] = true
	}
	for id := range r.errs {
		ids[id] = true
	}
	var retired []string
	for id := range r.retired {
		ids[id] = true
		retired = append(retired, id)
	}
	rep := &agentproto.Report{
		AgentVersion:    r.opts.AgentVersion,
		AppliedRevision: r.revision,
		ClockSkew:       r.skew,
	}
	subs := make([]agentproto.SubscriptionReport, 0, len(ids))
	for id := range ids {
		s := agentproto.SubscriptionReport{ID: id}
		if e, ok := r.applied[id]; ok {
			s.AppliedVersion = e.Version
		}
		if ae := r.errs[id]; ae != nil {
			c := *ae
			s.ApplyError = &c
		}
		subs = append(subs, s)
	}
	r.mu.Unlock()

	for i := range subs {
		subs[i].UsedTotal = r.agent.Used(subs[i].ID)
		subs[i].Conns = r.agent.ConnCount(subs[i].ID)
	}
	slices.SortFunc(subs, func(a, b agentproto.SubscriptionReport) int { return strings.Compare(a.ID, b.ID) })
	rep.Subscriptions = subs
	rep.PersistOK = r.agent.PersistOK()
	rep.NodeTime = time.Now().UTC()
	return rep, retired
}

// reportOnce 上报一次，并据回执估算时钟偏差（R-14.7）。成功后，已撤销订阅的最终用量已被确认，
// 从节点上删除。
func (r *Runtime) reportOnce(ctx context.Context) error {
	rep, retired := r.buildReport()
	sent := time.Now()
	ack, err := r.client.Report(ctx, rep)
	if err != nil {
		return err
	}
	received := time.Now()
	r.observeSkew(sent, received, ack.ServerTime)

	if len(retired) > 0 {
		r.applyMu.Lock()
		for _, id := range retired {
			r.mu.Lock()
			still := r.retired[id]
			if still {
				delete(r.retired, id)
			}
			r.mu.Unlock()
			if still {
				// 已撤销的订阅没有端口与连接，用量不会再变，上报值即最终值。
				if err := r.agent.Forget(id); err != nil {
					r.log.Warn("删除已撤销订阅失败", "subscription", id, "err", err)
				}
			}
		}
		r.applyMu.Unlock()
	}
	return nil
}

// observeSkew 按一次往返估算偏差；整个区间都超出 ±1 秒才告警（agent.EstimateClockSkew），
// 结果随下一次上报发送（Report.ClockSkew），状态变化时写日志。
func (r *Runtime) observeSkew(sent, received, server time.Time) {
	if server.IsZero() {
		return
	}
	sk := agent.EstimateClockSkew(sent, received, server, time.Millisecond)
	bad := sk.Exceeds(agent.MaxClockSkew)
	info := &agentproto.ClockSkew{
		LowMs:      sk.Low.Milliseconds(),
		HighMs:     sk.High.Milliseconds(),
		Exceeded:   bad,
		MeasuredAt: received.UTC(),
	}
	r.mu.Lock()
	was := r.skewBad
	r.skew, r.skewBad = info, bad
	r.mu.Unlock()
	switch {
	case bad && !was:
		r.log.Error("节点时钟偏差超过 1 秒，到期执行依赖本地时钟，请检查 NTP", "skew_ms", sk.Mid().Milliseconds(),
			"low_ms", info.LowMs, "high_ms", info.HighMs)
	case !bad && was:
		r.log.Info("节点时钟偏差恢复正常", "skew_ms", sk.Mid().Milliseconds())
	}
}
