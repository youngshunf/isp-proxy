// Package mail 是控制面的邮件发件箱（mail_outbox）与发送器（D-15）。
//
// 业务事务内调用 Enqueue 写入发件箱（与验证码等业务写入同时提交）；Worker 异步取出到期的邮件，
// 渲染模板后交给 Sender 投递，失败按指数退避重试，超过次数标记 failed。
//
// 发件箱里的模板数据（含验证码）用 secret.Box 加密保存（R-13.3：验证与找回材料按敏感凭据处理），
// 投递完成或最终失败后清空。
//
// 发送器：SMTPSender（生产，配置项）与 FileSender（开发与测试：把邮件写成 .eml 文件，不发真实邮件）。
package mail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/go-gost/gost/internal/control/secret"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/jackc/pgx/v5/pgconn"
)

// 模板名（mail_outbox.template）。
const (
	TemplateVerifyEmail   = "verify_email"          // 注册验证码：data.code
	TemplateResetPassword = "reset_password"        // 找回密码验证码：data.code
	TemplateSetPassword   = "set_password"          // 管理员代建用户首次设置密码：data.code
	TemplateAccountExists = "notice_account_exists" // 已注册邮箱再次注册时的提醒（不含验证码）
)

// Message 是待发送的邮件。
type Message struct {
	To       string
	Template string
	Data     map[string]string
}

// Execer 是 pgx.Tx 的子集。
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Enqueue 把邮件写入发件箱，返回邮件 ID。应在业务事务内调用。
func Enqueue(ctx context.Context, db Execer, box *secret.Box, m Message) (string, error) {
	if _, ok := templates[m.Template]; !ok {
		return "", fmt.Errorf("mail: 未知模板 %q", m.Template)
	}
	if !strings.Contains(m.To, "@") {
		return "", fmt.Errorf("mail: 收件人 %q 不是邮箱", m.To)
	}
	id := store.NewID("mail")
	payload, err := sealData(box, id, m.Data)
	if err != nil {
		return "", err
	}
	if _, err := db.Exec(ctx, `INSERT INTO mail_outbox (id, to_email, template, payload) VALUES ($1, $2, $3, $4)`,
		id, m.To, m.Template, payload); err != nil {
		return "", fmt.Errorf("mail: 写入发件箱: %w", err)
	}
	return id, nil
}

type sealed struct {
	Enc string `json:"enc"`
}

func sealData(box *secret.Box, id string, data map[string]string) ([]byte, error) {
	if data == nil {
		data = map[string]string{}
	}
	plain, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return json.Marshal(sealed{Enc: base64.StdEncoding.EncodeToString(box.Seal(plain, []byte("mail:"+id)))})
}

func openData(box *secret.Box, id string, payload []byte) (map[string]string, error) {
	var s sealed
	if err := json.Unmarshal(payload, &s); err != nil {
		return nil, err
	}
	if s.Enc == "" {
		return nil, errors.New("mail: 发件箱数据已清空")
	}
	ct, err := base64.StdEncoding.DecodeString(s.Enc)
	if err != nil {
		return nil, err
	}
	plain, err := box.Open(ct, []byte("mail:"+id))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	return out, json.Unmarshal(plain, &out)
}

// Rendered 是渲染好的邮件。
type Rendered struct {
	ID      string // 发件箱 ID，用作 Message-ID 与文件名
	From    string
	To      string
	Subject string
	Body    string // 纯文本
}

// Sender 投递一封邮件。返回错误时发件箱会稍后重试。
type Sender interface {
	Send(ctx context.Context, m *Rendered) error
}

// Brand 是邮件中使用的产品名与站点地址（配置项）。
type Brand struct {
	Name    string // 例如 “示例代理”
	SiteURL string // 例如 https://portal.example.com
}

type template struct {
	subject string
	body    func(b Brand, d map[string]string) string
}

var templates = map[string]template{
	TemplateVerifyEmail: {"邮箱验证码", func(b Brand, d map[string]string) string {
		return "您好：\n\n您正在注册 " + b.Name + "，邮箱验证码是：\n\n    " + d["code"] +
			"\n\n验证码 15 分钟内有效，只能使用一次。如果不是您本人操作，请忽略本邮件。\n"
	}},
	TemplateResetPassword: {"找回密码验证码", func(b Brand, d map[string]string) string {
		return "您好：\n\n您正在找回 " + b.Name + " 的登录密码，验证码是：\n\n    " + d["code"] +
			"\n\n验证码 15 分钟内有效，只能使用一次。设置新密码后，已登录的设备都需要重新登录。\n如果不是您本人操作，请忽略本邮件，您的密码不会改变。\n"
	}},
	TemplateSetPassword: {"设置登录密码", func(b Brand, d map[string]string) string {
		return "您好：\n\n管理员已为您开通 " + b.Name + " 账号。请打开 " + b.SiteURL + "/forgot-password ，用下面的验证码设置登录密码：\n\n    " +
			d["code"] + "\n\n验证码 15 分钟内有效，只能使用一次；过期后可在同一页面重新获取。\n"
	}},
	TemplateAccountExists: {"该邮箱已有账号", func(b Brand, d map[string]string) string {
		return "您好：\n\n有人用这个邮箱注册 " + b.Name + "，但该邮箱已经有账号。\n如果是您本人，请直接登录；忘记密码可以在 " +
			b.SiteURL + "/forgot-password 找回。\n如果不是您本人操作，请忽略本邮件。\n"
	}},
}

// Render 渲染模板。
func Render(b Brand, tmpl string, data map[string]string) (subject, body string, err error) {
	t, ok := templates[tmpl]
	if !ok {
		return "", "", fmt.Errorf("mail: 未知模板 %q", tmpl)
	}
	if b.Name == "" {
		b.Name = "ISP 代理"
	}
	return "[" + b.Name + "] " + t.subject, t.body(b, data), nil
}
