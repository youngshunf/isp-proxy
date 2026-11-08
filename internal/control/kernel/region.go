package kernel

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/go-gost/gost/internal/control/httpx"
)

// DirectRegion 是 OpenAPI DirectRegion：节点的直出出口地域（D-25）。直出代理前台不显示出口 IP，只显示它。
type DirectRegion struct {
	CountryCode string `json:"countryCode"` // ISO 3166-1 alpha-2 小写，用于国旗
	Name        string `json:"name"`        // 显示名称，如 新加坡、香港
}

var countryCodeRe = regexp.MustCompile(`^[a-z]{2}$`)

// Normalize 去空白、代码转小写并校验：代码为两位字母，名称 1～32 个字符。
func (d DirectRegion) Normalize() (DirectRegion, error) {
	d.CountryCode = strings.ToLower(strings.TrimSpace(d.CountryCode))
	d.Name = strings.TrimSpace(d.Name)
	if !countryCodeRe.MatchString(d.CountryCode) {
		return d, httpx.Invalid("出口地域代码须为两位字母（ISO 3166-1，如 sg、hk）")
	}
	if n := utf8.RuneCountInString(d.Name); n < 1 || n > 32 {
		return d, httpx.Invalid("出口地域名称为 1～32 个字符")
	}
	return d, nil
}

// RegionOf 由两列可空值组成地域；任一为空视为未设置。
func RegionOf(code, name *string) *DirectRegion {
	if code == nil || name == nil {
		return nil
	}
	return &DirectRegion{CountryCode: *code, Name: *name}
}

// OptRegion 是补丁里可清空的地域字段：缺省 = 不改；null = 清除；对象 = 设置。
type OptRegion struct {
	Set   bool
	Value *DirectRegion
}

// MarshalJSON 只写出值（审计记录补丁时使用）；未设置与清除都写 null。
func (o OptRegion) MarshalJSON() ([]byte, error) { return json.Marshal(o.Value) }

// UnmarshalJSON 区分 null 与对象，对象内同样拒绝未知字段。
func (o *OptRegion) UnmarshalJSON(b []byte) error {
	o.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		o.Value = nil
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var d DirectRegion
	if err := dec.Decode(&d); err != nil {
		return httpx.Invalid("directRegion 须为 {countryCode, name} 或 null")
	}
	o.Value = &d
	return nil
}
