package mail

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/control/secret"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/go-gost/gost/internal/control/store/pgtest"
	"github.com/jackc/pgx/v5"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func testBox(t *testing.T) *secret.Box {
	b, err := secret.NewBox(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOutboxFileDeliveryAndRetry(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	box := testBox(t)
	dir := t.TempDir()
	now := time.Now()
	fails := 1
	sender := &FileSender{Dir: dir, Fail: func(*Rendered) error {
		if fails > 0 {
			fails--
			return errors.New("SMTP 暂时不可用")
		}
		return nil
	}}
	w := &Worker{Pool: pool, Box: box, Sender: sender, From: "示例 <no-reply@example.com>", Brand: Brand{Name: "示例代理", SiteURL: "https://p.example.com"},
		Now: func() time.Time { return now }}

	var id string
	if err := store.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		id, err = Enqueue(ctx, tx, box, Message{To: "u@example.com", Template: TemplateVerifyEmail, Data: map[string]string{"code": "123456"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// 发件箱里不存明文验证码。
	var payload string
	_ = pool.QueryRow(ctx, `SELECT payload::text FROM mail_outbox WHERE id = $1`, id).Scan(&payload)
	if strings.Contains(payload, "123456") {
		t.Fatalf("发件箱明文保存了验证码: %s", payload)
	}

	now = time.Now().Add(time.Second)
	res, err := w.DeliverDue(ctx)
	if err != nil || res.Retried != 1 || res.Sent != 0 {
		t.Fatalf("首次应失败并等待重试: %+v %v", res, err)
	}
	res, _ = w.DeliverDue(ctx)
	if res != (Result{}) {
		t.Fatalf("退避期内不应重试: %+v", res)
	}
	now = now.Add(11 * time.Second)
	res, err = w.DeliverDue(ctx)
	if err != nil || res.Sent != 1 {
		t.Fatalf("退避后应投递成功: %+v %v", res, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, id+".eml"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.To != "u@example.com" || p.Subject != "[示例代理] 邮箱验证码" || !strings.Contains(p.Body, "    123456\n") {
		t.Fatalf("%+v", p)
	}
	var status string
	var attempts int
	_ = pool.QueryRow(ctx, `SELECT status, attempts, payload::text FROM mail_outbox WHERE id = $1`, id).Scan(&status, &attempts, &payload)
	if status != "sent" || attempts != 2 || payload != "{}" {
		t.Fatalf("%s %d %s", status, attempts, payload)
	}

	// 一直失败：达到上限后标记 failed 并清空数据。
	sender.Fail = func(*Rendered) error { return errors.New("拒收") }
	w.MaxAttempts = 2
	_ = store.InTx(ctx, pool, func(tx pgx.Tx) error {
		id, _ = Enqueue(ctx, tx, box, Message{To: "v@example.com", Template: TemplateResetPassword, Data: map[string]string{"code": "654321"}})
		return nil
	})
	// 退避从失败时刻算起：即使邮件创建已久，同一轮也不会连续重试。
	now = now.Add(time.Hour)
	res, _ = w.DeliverDue(ctx)
	if res.Retried != 1 {
		t.Fatalf("%+v", res)
	}
	now = now.Add(11 * time.Second)
	res, _ = w.DeliverDue(ctx)
	_ = pool.QueryRow(ctx, `SELECT status, attempts, payload::text FROM mail_outbox WHERE id = $1`, id).Scan(&status, &attempts, &payload)
	if res.Failed != 1 || status != "failed" || attempts != 2 || payload != "{}" {
		t.Fatalf("%+v %s %d %s", res, status, attempts, payload)
	}

	if _, err := Enqueue(ctx, pool, box, Message{To: "x@example.com", Template: "nope"}); err == nil {
		t.Fatal("未知模板应当报错")
	}
}

// fakeSMTP 是最小的 SMTP 服务器，记录收到的 DATA。
func fakeSMTP(t *testing.T) (addr string, got func() string) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var mu sync.Mutex
	var data strings.Builder
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		wr := func(s string) { c.Write([]byte(s + "\r\n")) }
		wr("220 fake")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					wr("250 ok")
					continue
				}
				mu.Lock()
				data.WriteString(line)
				mu.Unlock()
				continue
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"):
				wr("250-fake\r\n250 8BITMIME")
			case strings.HasPrefix(cmd, "DATA"):
				inData = true
				wr("354 go")
			case strings.HasPrefix(cmd, "QUIT"):
				wr("221 bye")
				return
			default:
				wr("250 ok")
			}
		}
	}()
	return l.Addr().String(), func() string { mu.Lock(); defer mu.Unlock(); return data.String() }
}

func TestSMTPSender(t *testing.T) {
	addr, got := fakeSMTP(t)
	s := &SMTPSender{Addr: addr, Security: SMTPPlain, Timeout: 5 * time.Second}
	err := s.Send(context.Background(), &Rendered{ID: "mail_x", From: "示例 <no-reply@example.com>", To: "u@example.com", Subject: "主题", Body: "验证码 111222\n"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse([]byte(got()))
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "主题" || !strings.Contains(p.Body, "111222") {
		t.Fatalf("%+v", p)
	}
	// 默认 STARTTLS：服务器不支持时拒绝明文发送。
	addr2, _ := fakeSMTP(t)
	s2 := &SMTPSender{Addr: addr2, Timeout: 5 * time.Second}
	if err := s2.Send(context.Background(), &Rendered{ID: "mail_y", From: "a@example.com", To: "u@example.com"}); err == nil {
		t.Fatal("不支持 STARTTLS 时应当失败")
	}
}
