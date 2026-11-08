package kernel

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/go-gost/gost/internal/control/httpx"
)

// 城市可选（D-27，docs/06 §10）：套餐与上游资源的城市可以为空。
// 套餐城市为空表示不限城市；资源城市为空表示供应商未给出城市。

// MaxCityRunes 是城市名称的最大字符数。
const MaxCityRunes = 64

// NormCity 去掉首尾空白；空串与纯空白归一为 nil（不限 / 未填）。
func NormCity(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}

// CheckCity 校验已归一的城市：为空合法，有值时不超过 MaxCityRunes 个字符。
func CheckCity(s *string) error {
	if s != nil && utf8.RuneCountInString(*s) > MaxCityRunes {
		return httpx.Invalid("城市名称不超过 64 个字符")
	}
	return nil
}

// CityLabel 是城市的显示文字：空为 def（套餐用“不限城市”）。
func CityLabel(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

// SameCity 报告两个已归一的城市是否相同（都为空也算相同）。
func SameCity(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// OptString 是补丁里可清空的字符串字段：缺省 = 不改；null = 清除；字符串 = 设置。
type OptString struct {
	Set   bool
	Value *string
}

// MarshalJSON 只写出值（审计记录补丁时使用）；未设置与清除都写 null。
func (o OptString) MarshalJSON() ([]byte, error) { return json.Marshal(o.Value) }

// UnmarshalJSON 区分 null 与字符串。
func (o *OptString) UnmarshalJSON(b []byte) error {
	o.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		o.Value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return httpx.Invalid("字段须为字符串或 null")
	}
	o.Value = &s
	return nil
}
