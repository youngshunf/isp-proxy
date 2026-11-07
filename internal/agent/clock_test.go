package agent

// A-W9：时钟偏差检测（R-14.7）。

import (
	"net/http"
	"testing"
	"time"
)

func TestEstimateClockSkew(t *testing.T) {
	server := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	rtt := 80 * time.Millisecond
	// 本地时钟比参考时钟快 skew；参考时间在往返中点打出，精度 1 ms。
	sample := func(skew time.Duration) ClockSkew {
		sent := server.Add(skew - rtt/2)
		return EstimateClockSkew(sent, sent.Add(rtt), server, time.Millisecond)
	}
	for _, c := range []struct {
		skew time.Duration
		want bool
	}{
		{0, false}, {200 * time.Millisecond, false}, {-800 * time.Millisecond, false},
		{1200 * time.Millisecond, true}, {3 * time.Second, true}, {-2500 * time.Millisecond, true},
	} {
		s := sample(c.skew)
		if s.Low > c.skew || s.High < c.skew {
			t.Errorf("skew %v outside estimate [%v, %v]", c.skew, s.Low, s.High)
		}
		if got := s.Exceeds(MaxClockSkew); got != c.want {
			t.Errorf("skew %v: Exceeds=%v, want %v (estimate [%v, %v])", c.skew, got, c.want, s.Low, s.High)
		}
		if d := s.Mid() - c.skew; d < -rtt || d > rtt {
			t.Errorf("skew %v: Mid %v", c.skew, s.Mid())
		}
	}
}

// HTTP Date 只有秒精度：只在偏差确定超过上限时告警，不对 ≤ 1 秒的偏差误报。
func TestClockSkewFromDateHeader(t *testing.T) {
	rtt := 20 * time.Millisecond
	check := func(serverNow time.Time, skew time.Duration) ClockSkew {
		t.Helper()
		h := http.Header{}
		h.Set("Date", serverNow.UTC().Format(http.TimeFormat)) // 截断到秒
		sent := serverNow.Add(skew - rtt/2)
		s, err := ClockSkewFromDate(h.Get("Date"), sent, sent.Add(rtt))
		if err != nil {
			t.Fatal(err)
		}
		if s.Low > skew || s.High < skew {
			t.Fatalf("skew %v outside estimate [%v, %v]", skew, s.Low, s.High)
		}
		return s
	}
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for _, phase := range []time.Duration{10 * time.Millisecond, 500 * time.Millisecond, 990 * time.Millisecond} {
		for _, skew := range []time.Duration{0, 900 * time.Millisecond, -time.Second} {
			if s := check(base.Add(phase), skew); s.Exceeds(MaxClockSkew) {
				t.Errorf("phase %v skew %v: false alarm [%v, %v]", phase, skew, s.Low, s.High)
			}
		}
		for _, skew := range []time.Duration{2500 * time.Millisecond, -2500 * time.Millisecond, time.Minute} {
			if s := check(base.Add(phase), skew); !s.Exceeds(MaxClockSkew) {
				t.Errorf("phase %v skew %v: not flagged [%v, %v]", phase, skew, s.Low, s.High)
			}
		}
	}
	if _, err := ClockSkewFromDate("yesterday", base, base); err == nil {
		t.Fatal("bad Date accepted")
	}
}
