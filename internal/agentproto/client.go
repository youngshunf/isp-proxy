package agentproto

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client 是 agent 侧的控制面客户端。可并发使用。
type Client struct {
	base string
	hc   *http.Client

	// MinBackoff、MaxBackoff 控制 Watch 出错后的重试间隔（默认 200ms～5s，指数增长）。
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

// NewClient 创建客户端。baseURL 例如 https://control.internal:8443；tlsCfg 由 ClientTLSConfig 构造。
func NewClient(baseURL string, tlsCfg *tls.Config) *Client {
	tr := &http.Transport{
		TLSClientConfig:     tlsCfg,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return NewClientWithHTTP(baseURL, &http.Client{Transport: tr})
}

// NewClientWithHTTP 使用调用方提供的 http.Client（测试或自定义传输）。不要设置 http.Client.Timeout，
// 否则会截断长轮询；每个请求的超时由本客户端按操作设置。
func NewClientWithHTTP(baseURL string, hc *http.Client) *Client {
	return &Client{
		base:       strings.TrimRight(baseURL, "/"),
		hc:         hc,
		MinBackoff: 200 * time.Millisecond,
		MaxBackoff: 5 * time.Second,
	}
}

// StatusError 是控制面返回的非成功状态。
type StatusError struct {
	Status int
	Body   Error
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("agentproto: 控制面返回 %d %s: %s", e.Status, e.Body.Code, e.Body.Message)
}

// Fetch 立即拉取完整期望状态（启动时使用）。
func (c *Client) Fetch(ctx context.Context) (*Desired, error) {
	return c.desired(ctx, "")
}

// Poll 长轮询：控制面修订号大于 after 时返回新的期望状态；等待超时（最长 MaxLongPoll）返回 (nil, nil)。
func (c *Client) Poll(ctx context.Context, after uint64) (*Desired, error) {
	return c.desired(ctx, strconv.FormatUint(after, 10))
}

func (c *Client) desired(ctx context.Context, after string) (*Desired, error) {
	// 比服务端最长等待多留余量，超时由服务端的 304 而不是客户端截断来结束。
	ctx, cancel := context.WithTimeout(ctx, MaxLongPoll+15*time.Second)
	defer cancel()
	u := c.base + PathDesired
	if after != "" {
		u += "?after=" + url.QueryEscape(after)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer drain(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		var d Desired
		if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
			return nil, fmt.Errorf("agentproto: 解析期望状态: %w", err)
		}
		return &d, nil
	case http.StatusNotModified:
		return nil, nil
	default:
		return nil, statusError(resp)
	}
}

// Report 上报观测值。上报是累计值，天然幂等，因此请求带 Idempotency-Key，传输层可在连接失效时安全重试。
func (c *Client) Report(ctx context.Context, r *Report) (*ReportAck, error) {
	var ack ReportAck
	if err := c.post(ctx, PathReport, r, &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

// PostProbeResults 回传探测结果；控制面按探测 ID 幂等。
func (c *Client) PostProbeResults(ctx context.Context, results []ProbeResult) error {
	return c.post(ctx, PathProbeResults, &ProbeResults{Results: results}, nil)
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// 让 net/http 把该 POST 视为可重放：复用的空闲连接已被服务端关闭（例如控制面重启）时自动重试一次。
	var k [12]byte
	_, _ = rand.Read(k[:])
	req.Header.Set("Idempotency-Key", hex.EncodeToString(k[:]))
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp.Body)
	if resp.StatusCode/100 != 2 {
		return statusError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("agentproto: 解析响应: %w", err)
	}
	return nil
}

func statusError(resp *http.Response) error {
	e := &StatusError{Status: resp.StatusCode}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, &e.Body) != nil || e.Body.Code == "" {
		e.Body = Error{Code: http.StatusText(resp.StatusCode), Message: strings.TrimSpace(string(b))}
	}
	return e
}

func drain(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	_ = rc.Close()
}

// Watch 持续跟随期望状态：先 Fetch 一次，之后以上次修订号长轮询；每拿到新状态就调用 fn。
// 网络错误与控制面 5xx 按指数退避重试，控制面重启后自动恢复；收到 401/403 时返回错误（证书或节点问题，重试无益）。
// fn 返回错误时 Watch 立即返回该错误。ctx 结束时返回 ctx.Err()。
func (c *Client) Watch(ctx context.Context, fn func(*Desired) error) error {
	var (
		have    bool
		after   uint64
		backoff = c.MinBackoff
	)
	for {
		var d *Desired
		var err error
		if have {
			d, err = c.Poll(ctx, after)
		} else {
			d, err = c.Fetch(ctx)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			var se *StatusError
			if errors.As(err, &se) && (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden) {
				return err
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff = min(backoff*2, c.MaxBackoff)
			continue
		}
		backoff = c.MinBackoff
		if d == nil {
			continue // 304：没有变化
		}
		have, after = true, d.Revision
		if err := fn(d); err != nil {
			return err
		}
	}
}
