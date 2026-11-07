package agent

// A-W5：换出口前的出口检测与比较（A-11、A-12、A-15）。

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// echoURL 的主机名无关紧要：伪上游忽略目标，一律连到自己的 backend。
const echoURL = "http://echo.test/ip"

func TestProbeExit(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<40, time.Hour) // 有客户订阅在跑，检测不能碰它的计量
	if _, _, err := fetch(pHTTP, acc, e.canary.addr, "/ip"); err != nil {
		t.Fatal(err)
	}

	echo := startBackend(t, "198.51.100.7") // /ip 返回该地址，模拟“目标侧看到的出口 IP”
	up := startUpstream(t, echo.addr, "cu", "cp")
	good := Exit{Version: 9, Protocol: "http", Addr: up.addr, Username: "cu", Password: "cp"}

	// 等客户连接在代理侧完全关闭，用量不再变化，再比较检测前后。
	eventually(t, time.Second, func() bool { return e.a.ConnCount("s1") == 0 }, "customer connection not closed")
	used, kicks, conns := e.a.Used("s1"), e.a.Kicks(), e.a.ConnCount("s1")
	res, err := ProbeExit(context.Background(), good, echoURL)
	if err != nil || res.Class != ReasonOK || res.IP != "198.51.100.7" || res.Latency <= 0 {
		t.Fatalf("probe: %+v %v", res, err)
	}
	if e.a.Used("s1") != used || e.a.Kicks() != kicks || e.a.ConnCount("s1") != conns {
		t.Fatal("probe touched a customer's meter or connections")
	}

	blackhole, err := net.Listen("tcp", "127.0.0.1:0") // 接受连接但从不应答
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	go func() {
		for {
			c, err := blackhole.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	deadBackendUp := startUpstream(t, freeAddr(t), "cu", "cp") // 上游可用，但它到目标的路不通
	notIP := startUpstream(t, e.backA.addr, "cu", "cp")        // 回显内容不是 IP

	cases := []struct {
		name string
		exit Exit
		want ExitReason
	}{
		{"wrong password", Exit{Protocol: "http", Addr: up.addr, Username: "cu", Password: "bad"}, ReasonAuthFailed},
		{"upstream down", Exit{Protocol: "http", Addr: freeAddr(t), Username: "cu", Password: "cp"}, ReasonUnreachable},
		{"upstream silent", Exit{Protocol: "http", Addr: blackhole.Addr().String(), Username: "cu", Password: "cp"}, ReasonUnreachable},
		{"upstream 502", Exit{Protocol: "http", Addr: deadBackendUp.addr, Username: "cu", Password: "cp"}, ReasonUpstreamError},
		{"echo not an IP", Exit{Protocol: "http", Addr: notIP.addr, Username: "cu", Password: "cp"}, ReasonBadResponse},
		{"bad protocol", Exit{Protocol: "nope", Addr: up.addr}, ReasonUpstreamError},
	}
	for _, c := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		start := time.Now()
		res, err := ProbeExit(ctx, c.exit, echoURL)
		cancel()
		if err == nil || res.Class != c.want {
			t.Errorf("%s: class %q err %v, want %q", c.name, res.Class, err, c.want)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s: took %v", c.name, d)
		}
	}
	if _, err := ProbeExit(context.Background(), good, "ftp://echo.test/"); err == nil {
		t.Error("non-HTTP echo URL accepted")
	}
	if e.a.Used("s1") != used || e.a.Kicks() != kicks {
		t.Fatal("failed probes touched a customer's meter")
	}
}

func TestCompareExit(t *testing.T) {
	countries := map[string]string{"198.51.100.7": "US", "203.0.113.9": "JP"}
	lookup := func(ip string) (string, error) {
		if c, ok := countries[ip]; ok {
			return c, nil
		}
		return "", errors.New("unknown ip")
	}
	base := ExitExpectation{
		CurrentIP: "203.0.113.1",
		Country:   "US",
		CountryOf: lookup,
		TakenIPs:  map[string]string{"203.0.113.9": "sub-other"},
	}
	ok := ProbeResult{Class: ReasonOK, IP: "198.51.100.7"}

	cases := []struct {
		name string
		res  ProbeResult
		mod  func(*ExitExpectation)
		want ExitReason
	}{
		{"pass", ok, nil, ReasonOK},
		{"auth failed", ProbeResult{Class: ReasonAuthFailed}, nil, ReasonAuthFailed},
		{"unreachable", ProbeResult{Class: ReasonUnreachable}, nil, ReasonUnreachable},
		{"ip unchanged", ok, func(x *ExitExpectation) { x.CurrentIP = "198.51.100.7" }, ReasonIPUnchanged},
		{"ip taken", ProbeResult{Class: ReasonOK, IP: "203.0.113.9"}, func(x *ExitExpectation) { x.Country = "JP" }, ReasonIPTaken},
		{"country mismatch", ok, func(x *ExitExpectation) { x.Country = "JP" }, ReasonCountryMismatch},
		{"country case-insensitive", ok, func(x *ExitExpectation) { x.Country = "us" }, ReasonOK},
		{"country unknown", ProbeResult{Class: ReasonOK, IP: "192.0.2.50"}, nil, ReasonCountryUnknown},
		{"no lookup", ok, func(x *ExitExpectation) { x.CountryOf = nil }, ReasonCountryUnknown},
		{"no country constraint", ok, func(x *ExitExpectation) { x.Country = ""; x.CountryOf = nil }, ReasonOK},
		{"first assignment", ok, func(x *ExitExpectation) { x.CurrentIP = "" }, ReasonOK},
		{"empty probe ip", ProbeResult{Class: ReasonOK}, nil, ReasonBadResponse},
	}
	for _, c := range cases {
		exp := base
		if c.mod != nil {
			c.mod(&exp)
		}
		v := CompareExit(c.res, exp)
		if v.Reason != c.want || v.OK != (c.want == ReasonOK) {
			t.Errorf("%s: %+v, want %q", c.name, v, c.want)
		}
		if !v.OK && v.Detail == "" {
			t.Errorf("%s: failing verdict without detail", c.name)
		}
	}
}
