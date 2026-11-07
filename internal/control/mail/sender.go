package mail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Format 生成 RFC 5322 邮件（UTF-8 纯文本，quoted-printable，主题按 RFC 2047 编码）。
func Format(m *Rendered, date time.Time) []byte {
	var b bytes.Buffer
	host := "isp-control.invalid"
	if i := strings.LastIndex(m.From, "@"); i >= 0 {
		host = strings.Trim(m.From[i+1:], "> ")
	}
	fmt.Fprintf(&b, "From: %s\r\n", m.From)
	fmt.Fprintf(&b, "To: %s\r\n", m.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.BEncoding.Encode("UTF-8", m.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", date.UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", m.ID, host)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(strings.ReplaceAll(m.Body, "\n", "\r\n")))
	_ = qp.Close()
	b.WriteString("\r\n")
	return b.Bytes()
}

// Parsed 是解析后的邮件（测试与平台 E2E 读取文件投递的邮件用）。
type Parsed struct {
	To, Subject, Body string
}

// Parse 解析 Format 生成的邮件。
func Parse(raw []byte) (*Parsed, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	subj, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		return nil, err
	}
	var body io.Reader = msg.Body
	if strings.EqualFold(msg.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
		body = quotedprintable.NewReader(msg.Body)
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	return &Parsed{To: msg.Header.Get("To"), Subject: subj, Body: strings.ReplaceAll(string(b), "\r\n", "\n")}, nil
}

// FileSender 把邮件写成 <Dir>/<ID>.eml（开发与测试用的“文件投递”，D-15）。
// Fail 非空时每次投递先调用它，返回错误即视为发送失败（测试发送失败与重试）。
type FileSender struct {
	Dir  string
	Fail func(m *Rendered) error
	Now  func() time.Time
}

// Send 实现 Sender：先写临时文件再改名，读取方不会看到写了一半的邮件。
func (s *FileSender) Send(_ context.Context, m *Rendered) error {
	if s.Fail != nil {
		if err := s.Fail(m); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	tmp, err := os.CreateTemp(s.Dir, ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(Format(m, now())); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.Dir, m.ID+".eml"))
}

// SMTP 安全模式。
const (
	SMTPStartTLS = "starttls" // 明文连接后 STARTTLS（默认，常见 587 端口）；服务器不支持时失败
	SMTPTLS      = "tls"      // 隐式 TLS（常见 465 端口）
	SMTPPlain    = "plain"    // 不加密，只用于本机中继
)

// SMTPSender 通过 SMTP 投递。Password 只来自环境变量（ISP_SMTP_PASSWORD，docs/06 §2）。
type SMTPSender struct {
	Addr     string // host:port
	Security string // SMTPStartTLS（默认）、SMTPTLS、SMTPPlain
	Username string
	Password string
	Timeout  time.Duration // 默认 30 秒
}

// Send 实现 Sender。
func (s *SMTPSender) Send(ctx context.Context, m *Rendered) error {
	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return fmt.Errorf("mail: SMTP 地址 %q: %w", s.Addr, err)
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d := &net.Dialer{}
	var conn net.Conn
	tlsCfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if s.Security == SMTPTLS {
		conn, err = (&tls.Dialer{NetDialer: d, Config: tlsCfg}).DialContext(ctx, "tcp", s.Addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", s.Addr)
	}
	if err != nil {
		return fmt.Errorf("mail: 连接 SMTP: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mail: SMTP 握手: %w", err)
	}
	defer c.Close()
	if s.Security == "" || s.Security == SMTPStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("mail: SMTP 服务器不支持 STARTTLS")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("mail: STARTTLS: %w", err)
		}
	}
	if s.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, host)); err != nil {
			return fmt.Errorf("mail: SMTP 认证: %w", err)
		}
	}
	from, err := mail.ParseAddress(m.From)
	if err != nil {
		return fmt.Errorf("mail: 发件人 %q: %w", m.From, err)
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(m.To); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	if _, err := bw.Write(Format(m, time.Now())); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
