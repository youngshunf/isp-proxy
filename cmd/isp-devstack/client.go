package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"

	"github.com/go-gost/gost/internal/control/admin"
)

// apiClient 是带 Cookie 与 CSRF 双提交的最小 JSON 客户端（管理端 /admin-api）。
type apiClient struct {
	base   string
	prefix string
	hc     *http.Client
}

func newAPIClient(base, prefix string) *apiClient {
	jar, _ := cookiejar.New(nil)
	return &apiClient{base: base, prefix: prefix, hc: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

type apiError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s: HTTP %d %s %s", e.Path, e.Status, e.Code, e.Message)
}

func (c *apiClient) csrf() string {
	u, _ := url.Parse(c.base + "/")
	for _, ck := range c.hc.Jar.Cookies(u) {
		if ck.Name == "isp_csrf" {
			return ck.Value
		}
	}
	_ = c.do("GET", "/me", nil, nil, "")
	for _, ck := range c.hc.Jar.Cookies(u) {
		if ck.Name == "isp_csrf" {
			return ck.Value
		}
	}
	return ""
}

func newKey() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "devstack-" + hex.EncodeToString(b[:])
}

func (c *apiClient) do(method, path string, in, out any, idem string) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+c.prefix+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != "GET" {
		req.Header.Set("X-CSRF-Token", c.csrf())
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode/100 != 2 {
		ae := &apiError{Status: res.StatusCode, Path: method + " " + c.prefix + path}
		_ = json.Unmarshal(raw, ae)
		if ae.Message == "" {
			ae.Message = string(raw)
		}
		return ae
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// adminLogin 完成两步登录（首次登录绑定 TOTP），返回 Base32 密钥与本次使用的时间步。
func adminLogin(c *apiClient, email, password string, secret string, lastStep int64) (string, int64, error) {
	var ch struct {
		Stage      string `json:"stage"`
		Enrollment *struct {
			Secret string `json:"secret"`
		} `json:"enrollment"`
	}
	if err := c.do("POST", "/sessions", map[string]string{"email": email, "password": password}, &ch, ""); err != nil {
		return "", 0, err
	}
	if ch.Enrollment != nil {
		secret = ch.Enrollment.Secret
	}
	if secret == "" {
		return "", 0, fmt.Errorf("管理员 %s 已绑定 TOTP，但不知道密钥", email)
	}
	key, err := admin.DecodeSecret(secret)
	if err != nil {
		return "", 0, err
	}
	// 同一时间步只能用一次（防重放），允许 +1 步。
	for {
		step := admin.TOTPStep(time.Now())
		if step <= lastStep {
			if lastStep+1 > step+1 {
				time.Sleep(time.Second)
				continue
			}
			step = lastStep + 1
		}
		if err := c.do("POST", "/sessions/current/mfa", map[string]string{"code": admin.HOTP(key, uint64(step), 6)}, nil, ""); err != nil {
			return "", 0, err
		}
		return secret, step, nil
	}
}
