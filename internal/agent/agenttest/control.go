package agenttest

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
)

// Control 是伪控制面：用 agentproto.NewHandler 与开发证书在 127.0.0.1 上提供 mTLS 节点接口，
// 保存一份可修改的期望状态，按 GREATEST 合并上报，按探测 ID 幂等记录探测结果。
// 可以停止与在同一地址重启，模拟控制面离线。
type Control struct {
	NodeID string
	Certs  *agentproto.DevCerts
	Addr   string // 重启后保持不变

	t       testing.TB
	hub     *agentproto.Hub
	handler http.Handler
	tlsCfg  *tls.Config

	mu         sync.Mutex
	desired    agentproto.Desired
	reports    []agentproto.Report
	used       map[string]uint64
	results    map[string]agentproto.ProbeResult
	posts      map[string]int
	offset     time.Duration
	srv        *http.Server
	reportHook func(*agentproto.Report)
}

// StartControl 生成开发证书并启动伪控制面。长轮询最长等待 maxWait（测试用短值）。
func StartControl(t testing.TB, nodeID string, maxWait time.Duration) *Control {
	t.Helper()
	certs, err := agentproto.GenerateDevCerts(t.TempDir(), agentproto.DevCertOptions{NodeIDs: []string{nodeID}})
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := agentproto.LoadServerTLSConfig(certs.CA.Cert, certs.Server.Cert, certs.Server.Key)
	if err != nil {
		t.Fatal(err)
	}
	c := &Control{
		NodeID:  nodeID,
		Certs:   certs,
		t:       t,
		hub:     agentproto.NewHub(),
		tlsCfg:  tlsCfg,
		desired: agentproto.Desired{Revision: 1, NodeID: nodeID, Subscriptions: []agentproto.SubscriptionEntry{}, Probes: []agentproto.Probe{}},
		used:    map[string]uint64{},
		results: map[string]agentproto.ProbeResult{},
		posts:   map[string]int{},
	}
	c.handler = agentproto.NewHandler(c, c.hub, agentproto.HandlerOptions{MaxWait: maxWait})
	c.Start()
	t.Cleanup(c.Stop)
	return c
}

// URL 返回控制面地址（https://127.0.0.1:port）。
func (c *Control) URL() string { return "https://" + c.Addr }

// NodeCert 返回节点证书与私钥路径。
func (c *Control) NodeCert() agentproto.CertKeyPaths { return c.Certs.Nodes[c.NodeID] }

// Start 启动（或在原地址重启）控制面。
func (c *Control) Start() {
	c.t.Helper()
	addr := c.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var ln net.Listener
	var err error
	for i := 0; i < 100; i++ { // 重启时端口可能短暂不可用
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		c.t.Fatal(err)
	}
	c.Addr = ln.Addr().String()
	srv := &http.Server{Handler: c.handler, TLSConfig: c.tlsCfg, ErrorLog: log.New(io.Discard, "", 0)}
	c.mu.Lock()
	c.srv = srv
	c.mu.Unlock()
	go srv.ServeTLS(ln, "", "")
}

// Stop 关闭控制面与全部连接（含正在长轮询的连接）。可重复调用。
func (c *Control) Stop() {
	c.mu.Lock()
	srv := c.srv
	c.srv = nil
	c.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
}

// Update 修改期望状态并使修订号加一，唤醒长轮询。
func (c *Control) Update(fn func(d *agentproto.Desired)) {
	c.mu.Lock()
	fn(&c.desired)
	c.desired.Revision++
	c.mu.Unlock()
	c.hub.Notify(c.NodeID)
}

// Revision 返回当前修订号。
func (c *Control) Revision() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.desired.Revision
}

// SetClockOffset 让回执的 ServerTime 偏移 d（模拟节点时钟偏差为 -d）。
func (c *Control) SetClockOffset(d time.Duration) {
	c.mu.Lock()
	c.offset = d
	c.mu.Unlock()
}

// OnReport 设置每次收到上报时的回调（在锁外调用前已复制）。
func (c *Control) OnReport(fn func(*agentproto.Report)) {
	c.mu.Lock()
	c.reportHook = fn
	c.mu.Unlock()
}

// ---- agentproto.Server ----

func (c *Control) Desired(_ context.Context, node string) (*agentproto.Desired, error) {
	if node != c.NodeID {
		return nil, agentproto.ErrUnknownNode
	}
	c.mu.Lock()
	b, err := json.Marshal(c.desired) // 深拷贝，之后的 Update 不影响正在编码的响应
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var d agentproto.Desired
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	d.GeneratedAt = time.Now().UTC()
	return &d, nil
}

func (c *Control) Report(_ context.Context, node string, r *agentproto.Report) (*agentproto.ReportAck, error) {
	if node != c.NodeID {
		return nil, agentproto.ErrUnknownNode
	}
	c.mu.Lock()
	c.reports = append(c.reports, *r)
	for _, s := range r.Subscriptions {
		c.used[s.ID] = max(c.used[s.ID], s.UsedTotal) // GREATEST
	}
	ack := &agentproto.ReportAck{Revision: c.desired.Revision, ServerTime: time.Now().Add(c.offset).UTC()}
	hook := c.reportHook
	c.mu.Unlock()
	if hook != nil {
		hook(r)
	}
	return ack, nil
}

func (c *Control) ProbeResults(_ context.Context, node string, rs []agentproto.ProbeResult) error {
	if node != c.NodeID {
		return agentproto.ErrUnknownNode
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range rs {
		c.posts[r.ProbeID]++
		if _, done := c.results[r.ProbeID]; !done {
			c.results[r.ProbeID] = r
		}
	}
	return nil
}

// ---- 观察 ----

// Reports 返回收到的全部上报（副本）。
func (c *Control) Reports() []agentproto.Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]agentproto.Report(nil), c.reports...)
}

// Used 返回按 GREATEST 合并后的订阅累计用量。
func (c *Control) Used(id string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used[id]
}

// LastSub 返回最近一次上报中该订阅的条目。
func (c *Control) LastSub(id string) (agentproto.SubscriptionReport, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.reports) - 1; i >= 0; i-- {
		for _, s := range c.reports[i].Subscriptions {
			if s.ID == id {
				return s, true
			}
		}
		return agentproto.SubscriptionReport{}, false
	}
	return agentproto.SubscriptionReport{}, false
}

// ProbeResult 返回某探测的首个结果与累计回传次数。
func (c *Control) ProbeResult(id string) (agentproto.ProbeResult, int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.results[id]
	return r, c.posts[id], ok
}

// WaitReport 等待一次满足 cond 的上报（只看等待开始之后收到的上报），超时 Fatal。
func (c *Control) WaitReport(t testing.TB, max time.Duration, cond func(*agentproto.Report) bool) agentproto.Report {
	t.Helper()
	c.mu.Lock()
	from := len(c.reports)
	c.mu.Unlock()
	deadline := time.Now().Add(max)
	for {
		c.mu.Lock()
		for i := from; i < len(c.reports); i++ {
			if cond(&c.reports[i]) {
				r := c.reports[i]
				c.mu.Unlock()
				return r
			}
		}
		from = len(c.reports)
		c.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no matching report within %v", max)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// SubIn 在上报中查找订阅条目。
func SubIn(r *agentproto.Report, id string) (agentproto.SubscriptionReport, bool) {
	for _, s := range r.Subscriptions {
		if s.ID == id {
			return s, true
		}
	}
	return agentproto.SubscriptionReport{}, false
}

var _ agentproto.Server = (*Control)(nil)
