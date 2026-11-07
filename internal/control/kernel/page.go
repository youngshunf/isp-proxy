package kernel

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-gost/gost/internal/control/httpx"
)

// Page 是管理端分页响应（docs/06 §8.2：cursor + limit ≤ 200）。
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

// Cursor 是按 (排序键, 时间, ID) 的键集分页游标。
type Cursor struct {
	Rank int
	At   time.Time
	ID   string
}

// Encode 编码游标。
func (c Cursor) Encode() string {
	s := strconv.Itoa(c.Rank) + "|" + strconv.FormatInt(c.At.UnixMicro(), 10) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// ParsePage 解析 cursor 与 limit；cursor 为空返回 nil。
func ParsePage(r *http.Request) (*Cursor, int, error) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			return nil, 0, httpx.Invalid("limit 必须是 1～200 的整数")
		}
		limit = n
	}
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return nil, limit, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, 0, httpx.Invalid("cursor 不合法")
	}
	parts := strings.SplitN(string(b), "|", 3)
	if len(parts) != 3 {
		return nil, 0, httpx.Invalid("cursor 不合法")
	}
	rank, err1 := strconv.Atoi(parts[0])
	us, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return nil, 0, httpx.Invalid("cursor 不合法")
	}
	return &Cursor{Rank: rank, At: time.UnixMicro(us).UTC(), ID: parts[2]}, limit, nil
}

// ListParam 解析逗号分隔的数组查询参数（style: form, explode: false），并校验取值。
func ListParam(r *http.Request, name string, allowed ...string) ([]string, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(allowed) > 0 && !ok[p] {
			return nil, httpx.Invalid(name + " 取值不合法: " + p)
		}
		out = append(out, p)
	}
	return out, nil
}

// TimeParam 解析 RFC 3339 时间查询参数；缺省返回 nil。
func TimeParam(r *http.Request, name string) (*time.Time, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, httpx.Invalid(name + " 必须是 RFC 3339 时间")
	}
	t = t.UTC()
	return &t, nil
}

// Args 是构造动态 SQL 条件的小工具。
type Args struct {
	Where []string
	Vals  []any
}

// Add 追加一个条件，cond 中用 ? 表示参数占位（只能出现一次）。
func (a *Args) Add(cond string, v any) {
	a.Vals = append(a.Vals, v)
	a.Where = append(a.Where, strings.Replace(cond, "?", "$"+strconv.Itoa(len(a.Vals)), 1))
}

// AddRaw 追加不带参数的条件。
func (a *Args) AddRaw(cond string) { a.Where = append(a.Where, cond) }

// Next 返回下一个占位符并登记参数。
func (a *Args) Next(v any) string {
	a.Vals = append(a.Vals, v)
	return "$" + strconv.Itoa(len(a.Vals))
}

// SQL 返回 " WHERE …"（无条件时为空串）。
func (a *Args) SQL() string {
	if len(a.Where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(a.Where, " AND ")
}
