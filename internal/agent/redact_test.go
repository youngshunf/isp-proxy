package agent

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-gost/core/logger"
	xlogger "github.com/go-gost/x/logger"
)

// syncBuf 是并发安全的日志缓冲。
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func traceLogger(w *syncBuf) logger.Logger {
	return xlogger.NewLogger(xlogger.OutputOption(w), xlogger.LevelOption(logger.TraceLevel), xlogger.FormatOption(logger.JSONFormat))
}

// 客户发送的原始内容（请求行、头、认证明文）不得进日志；地址、服务名等诊断信息原样保留（A-18）。
func TestRedactLoggerMessages(t *testing.T) {
	var buf syncBuf
	l := newRedactLogger(traceLogger(&buf)).WithFields(map[string]any{"service": "acc-s1-v1-http", "user": "JUNKUSER\x00\xff"})
	secret := "JUNKMARK-SECRET"
	l.Error(errors.New(`malformed HTTP request "` + secret + `\x00\xff"`))
	l.Errorf("read: %v", fmt.Errorf("malformed MIME header line: %q", secret+"\x00"))
	l.Error(errors.New(`malformed HTTP version "` + secret + `"`))
	l.Trace("CONNECT x:1 HTTP/1.1\r\nProxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("u:"+secret)) + "\r\n")
	l.Tracef("%v", struct{ User, Pass string }{"u", secret})
	l.Info("127.0.0.1:1 <> 127.0.0.1:2")
	l.Error("init: ", errors.New("listen tcp4 127.0.0.1:30000: bind: address already in use"))
	out := buf.String()
	for _, leak := range []string{secret, "JUNKUSER", base64.StdEncoding.EncodeToString([]byte("u:" + secret))} {
		if strings.Contains(out, leak) {
			t.Fatalf("日志含客户内容 %q：\n%s", leak, out)
		}
	}
	for _, keep := range []string{"malformed HTTP request", "127.0.0.1:1 <> 127.0.0.1:2", "listen tcp4 127.0.0.1:30000: bind: address already in use",
		"acc-s1-v1-http", "已脱敏"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("日志缺少诊断信息 %q：\n%s", keep, out)
		}
	}
	if n := strings.Count(out, "\n"); n != 7 {
		t.Fatalf("应有 7 行日志，得到 %d：\n%s", n, out)
	}
}

// useTraceBase 让注册表里的脱敏日志器（底层为空）输出到 buf，测试结束恢复。
func useTraceBase(t *testing.T, buf *syncBuf) {
	l := traceLogger(buf)
	redactBase.Store(&l)
	t.Cleanup(func() { redactBase.Store(nil) })
}

// 默认日志器在注册之后才设置：底层为空的脱敏日志器在使用时才取底层日志器。
func TestRedactLoggerResolvesBaseLazily(t *testing.T) {
	var buf syncBuf
	l := newRedactLogger(nil)
	useTraceBase(t, &buf)
	l.WithFields(map[string]any{"k": "v"}).Error(`malformed HTTP request "JUNKMARK"`)
	if out := buf.String(); strings.Contains(out, "JUNKMARK") || !strings.Contains(out, "malformed HTTP request") {
		t.Fatalf("输出：%s", out)
	}
}

// 真实 GOST 服务（http、https、socks5）在 trace 级别下：错误密码、垃圾数据、畸形请求都不把客户内容写进日志。
func TestServicesDoNotLogClientBytes(t *testing.T) {
	var buf syncBuf
	useTraceBase(t, &buf)

	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<30, time.Hour)
	if got, _, err := fetch(pHTTP, acc, e.canary.addr, "/ip"); err != nil || got != "exit-A" {
		t.Fatalf("正常请求：%q %v", got, err)
	}
	const mark = "JUNKMARK7f3a"
	wrong := "WRONGPASS9c1d"
	for _, p := range []proto{pHTTP, pHTTPS, pSOCKS5} {
		if c, err := tunnel(p, p.addr(acc), acc.Username, wrong, e.canary.addr); err == nil {
			c.Close()
			t.Fatalf("%s：错误密码通过了认证", p)
		}
		if c, err := tunnel(p, p.addr(acc), mark+"user", mark+"pass", e.canary.addr); err == nil {
			c.Close()
		}
	}
	for _, payload := range []string{
		mark + "\x00\xff\r\n\r\n",
		"GET http://x/ HTTP/1.1\r\n" + mark + "\r\n\r\n",
		"GET http://x/ " + mark + "\r\n\r\n",
		"CONNECT x:1 HTTP/1.1\r\nProxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(mark+":"+mark)) + "\r\n\r\n",
		"\x05\x01\x02\x01\x0c" + mark + "\x0c" + mark,
	} {
		for _, addr := range []string{acc.HTTPAddr, acc.SOCKS5Addr} {
			c, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			c.SetDeadline(time.Now().Add(time.Second))
			c.Write([]byte(payload))
			b := make([]byte, 512)
			for {
				if _, err := c.Read(b); err != nil {
					break
				}
			}
			c.Close()
		}
	}
	time.Sleep(200 * time.Millisecond)
	out := buf.String()
	if !strings.Contains(out, "acc-s1-v1-http") {
		t.Fatalf("服务日志没有走默认日志器（测试无效）：\n%s", out)
	}
	for _, leak := range []string{mark, wrong, acc.Password, base64.StdEncoding.EncodeToString([]byte(acc.Username + ":" + acc.Password))} {
		if strings.Contains(out, leak) {
			i := strings.Index(out, leak)
			t.Fatalf("日志含客户内容 %q：…%s…", leak, out[max(0, i-300):min(len(out), i+100)])
		}
	}
}
