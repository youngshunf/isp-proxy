package agent

import (
	"net/http"
	"time"
)

// MaxClockSkew 是允许的节点时钟偏差（R-14.7）。到期在节点按本地时钟执行，
// 偏差超过它时应告警（C-W9 的“时钟偏差”事件）。
const MaxClockSkew = time.Second

// ClockSkew 是一次往返测得的本地时钟偏差区间：本地时钟减参考时钟，真实值落在 [Low, High]。
type ClockSkew struct {
	Low, High time.Duration
}

// EstimateClockSkew 根据一次请求估计偏差。sent、received 是本地发出请求与收到应答的时刻，
// ref 是参考方（控制面）在处理请求期间打出的时间，precision 是 ref 的精度（截断粒度）。
//
// 参考方打时间的那一刻在本地时钟上落在 [sent, received]，参考时钟上落在 [ref, ref+precision)，
// 因此偏差落在 [sent-(ref+precision), received-ref]。
func EstimateClockSkew(sent, received, ref time.Time, precision time.Duration) ClockSkew {
	if received.Before(sent) {
		sent, received = received, sent
	}
	return ClockSkew{Low: sent.Sub(ref.Add(precision)), High: received.Sub(ref)}
}

// ClockSkewFromDate 用 HTTP Date 头（秒精度）估计偏差。
//
// 秒精度意味着区间宽度至少 1 秒：Exceeds(MaxClockSkew) 不会对 ≤ 1 秒的偏差误报，
// 但只保证发现超过约 2 秒的偏差。要严格按 1 秒告警，控制面应在应答中带毫秒级时间，
// 改用 EstimateClockSkew（agentproto 上报里已有节点时钟，控制面一侧也可以比较）。
func ClockSkewFromDate(date string, sent, received time.Time) (ClockSkew, error) {
	ref, err := http.ParseTime(date)
	if err != nil {
		return ClockSkew{}, err
	}
	return EstimateClockSkew(sent, received, ref, time.Second), nil
}

// Mid 返回区间中点，作为偏差的点估计（展示用）。
func (s ClockSkew) Mid() time.Duration { return s.Low + (s.High-s.Low)/2 }

// Exceeds 报告偏差是否确定超出 ±limit：整个区间都在 [-limit, limit] 之外才算，避免误报。
func (s ClockSkew) Exceeds(limit time.Duration) bool {
	return s.Low > limit || s.High < -limit
}
