//go:build ispe2e

package isp

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-gost/gost/internal/control/mail"
)

// User 是用户端客户端（/api）。
type User struct {
	*Client
	e        *Env
	Email    string
	Password string
	ID       string
}

// NewClientUser 返回一个未登录的用户客户端（来源 IP 各不相同）。
func (e *Env) NewClientUser(email, password string) *User {
	n := e.userSeq.Add(1)
	c := newClient(e.t, e.Base, "/api")
	c.XFF = fmt.Sprintf("198.18.%d.%d", n/250, n%250+1)
	return &User{Client: c, e: e, Email: email, Password: password}
}

// NewUser 注册、读邮件验证码完成验证（验证即登录），返回已登录的已验证用户。
func (e *Env) NewUser() *User {
	e.t.Helper()
	n := e.userSeq.Load() + 1
	u := e.NewClientUser(fmt.Sprintf("user%d-%s@e2e.test", n, randHex(3)), "user-pass-"+randHex(5))
	u.Register()
	code := e.NextCode(u.Email, "邮箱验证码")
	var acc Account
	u.Must("POST", "/account/verify-email", map[string]string{"email": u.Email, "code": code}, &acc)
	if !acc.Verified {
		e.t.Fatalf("验证后账号未验证：%+v", acc)
	}
	u.ID = e.Admin.UserID(u.Email)
	return u
}

// Register 提交注册。
func (u *User) Register() {
	u.t.Helper()
	u.Must("POST", "/account/register", map[string]string{"email": u.Email, "password": u.Password}, nil)
}

// Login 登录。
func (u *User) Login() error {
	_, err := u.Do("POST", "/sessions", map[string]string{"email": u.Email, "password": u.Password}, nil)
	return err
}

// Subs 列出本人订阅。
func (u *User) Subs() []Subscription {
	u.t.Helper()
	var out []Subscription
	u.Get("/subscriptions", &out)
	return out
}

// Sub 读取本人订阅。
func (u *User) Sub(id string) Subscription {
	u.t.Helper()
	var s Subscription
	u.Get("/subscriptions/"+id, &s)
	return s
}

// Catalog 读取目录。
func (u *User) Catalog() Catalog {
	u.t.Helper()
	var c Catalog
	u.Get("/catalog", &c)
	return c
}

// Order 下单。
func (u *User) Order(in OrderInput, key string) (Order, error) {
	var o Order
	if key == "" {
		key = NewKey()
	}
	_, err := u.Do("POST", "/orders", in, &o, Idem(key))
	return o, err
}

// ChangeAccess 自助换接入（mode：password / ports_and_password）。
func (u *User) ChangeAccess(subID, mode string) (Access, error) {
	var a Access
	_, err := u.Do("POST", "/subscriptions/"+subID+"/access-changes", map[string]string{"mode": mode}, &a, Idem(NewKey()))
	return a, err
}

// ---- 邮件（文件投递） ----

var codeRe = regexp.MustCompile(`(?m)^\s+([0-9]{6})\s*$`)

// Mails 返回发给 email 的邮件（按文件修改时间排序），附文件名。
func (e *Env) Mails(email string) ([]*mail.Parsed, []string) {
	ents, _ := os.ReadDir(e.MailDir)
	type item struct {
		p    *mail.Parsed
		name string
		mt   time.Time
	}
	var items []item
	for _, de := range ents {
		if !strings.HasSuffix(de.Name(), ".eml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(e.MailDir, de.Name()))
		if err != nil {
			continue
		}
		p, err := mail.Parse(b)
		if err != nil || !strings.Contains(strings.ToLower(p.To), strings.ToLower(email)) {
			continue
		}
		info, _ := de.Info()
		items = append(items, item{p, de.Name(), info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mt.Before(items[j].mt) })
	var ps []*mail.Parsed
	var names []string
	for _, it := range items {
		ps = append(ps, it.p)
		names = append(names, it.name)
	}
	return ps, names
}

// NextMail 等待一封尚未读过的、主题含 subject 的邮件。
func (e *Env) NextMail(email, subject string) *mail.Parsed {
	e.t.Helper()
	return e.nextMail(email, subject, 20*time.Second)
}

func (e *Env) nextMail(email, subject string, max time.Duration) *mail.Parsed {
	e.t.Helper()
	var got *mail.Parsed
	e.Eventually(max, func() bool {
		ps, names := e.Mails(email)
		for i, p := range ps {
			key := names[i]
			if _, seen := e.mailSeen.Load(key); seen {
				continue
			}
			if subject != "" && !strings.Contains(p.Subject, subject) {
				continue
			}
			e.mailSeen.Store(key, true)
			got = p
			return true
		}
		return false
	}, "没有收到发给 %s、主题含 %q 的新邮件", email, subject)
	return got
}

// NextCode 等待新邮件并取出 6 位验证码。
func (e *Env) NextCode(email, subject string) string {
	e.t.Helper()
	return e.NextCodeWithin(email, subject, 20*time.Second)
}

// NextCodeWithin 同 NextCode，可指定等待上限。
func (e *Env) NextCodeWithin(email, subject string, max time.Duration) string {
	e.t.Helper()
	p := e.nextMail(email, subject, max)
	m := codeRe.FindStringSubmatch(p.Body)
	if m == nil {
		e.t.Fatalf("邮件里没有验证码：%q", p.Body)
	}
	return m[1]
}
