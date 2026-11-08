package geoip

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testdata/test-country.mmdb 由 mmdbwriter 生成（不进依赖）：
//
//	203.0.113.0/24  country=US
//	2001:db8:1::/48 country=JP
//	198.51.100.0/24 只有 registered_country=DE
const fixture = "testdata/test-country.mmdb"

func TestOverrides(t *testing.T) {
	o, err := ParseOverrides(map[string]string{
		"149.119.188.22":     "US",
		"2001:DB8:0:0::5":    "jp",
		"2001:db8:aa::/48":   "us",
		"2001:db8:aa:1::/64": "ca",
		"::ffff:192.0.2.9":   "gb",
	})
	if err != nil {
		t.Fatal(err)
	}
	l := Lookup{Overrides: o}
	for ip, want := range map[string]string{
		"149.119.188.22":        "us",
		"2001:db8::5":           "jp", // 不同写法也命中
		"2001:db8:aa:9::1":      "us",
		"2001:db8:aa:1::abcd":   "ca", // 最长前缀优先
		"192.0.2.9":             "gb", // IPv4 映射写法规范成 IPv4
		"::ffff:149.119.188.22": "us",
	} {
		if got, ok := l.Country(context.Background(), ip); !ok || got != want {
			t.Errorf("Country(%s) = %q,%v，期望 %q", ip, got, ok, want)
		}
	}
	if _, ok := l.Country(context.Background(), "10.0.0.1"); ok {
		t.Error("未列出的 IP 应为未知")
	}
	for _, bad := range []map[string]string{{"1.2.3": "us"}, {"1.2.3.4": "usa"}, {"1.2.3.0/33": "us"}} {
		if _, err := ParseOverrides(bad); err == nil {
			t.Errorf("ParseOverrides(%v) 应报错", bad)
		}
	}
}

func TestDBAndPrecedence(t *testing.T) {
	db := OpenDB(fixture, nil)
	defer db.Close()
	if !db.Loaded() {
		t.Fatal("测试库未加载")
	}
	o, _ := ParseOverrides(map[string]string{"203.0.113.7": "ca"})
	l := Lookup{Overrides: o, DB: db}
	for ip, want := range map[string]string{
		"203.0.113.8":    "us",
		"203.0.113.7":    "ca", // 手工映射优先于库
		"2001:db8:1::42": "jp",
		"198.51.100.1":   "de", // 只有注册国家时用注册国家
	} {
		if got, ok := l.Country(context.Background(), ip); !ok || got != want {
			t.Errorf("Country(%s) = %q,%v，期望 %q", ip, got, ok, want)
		}
	}
	if _, ok := l.Country(context.Background(), "192.0.2.1"); ok {
		t.Error("库里没有的 IP 应为未知")
	}
}

// 库文件缺失时返回未知；之后放上文件，到了检查周期自动加载，无需重启。
func TestDBLoadsLater(t *testing.T) {
	path := filepath.Join(t.TempDir(), "country.mmdb")
	now := time.Unix(1_800_000_000, 0)
	db := &DB{path: path, every: time.Minute, now: func() time.Time { return now }}
	db.log = slog.New(slog.DiscardHandler)
	db.reload()
	defer db.Close()
	l := Lookup{DB: db}
	if _, ok := l.Country(context.Background(), "203.0.113.8"); ok {
		t.Fatal("没有库文件时应为未知")
	}
	b, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Country(context.Background(), "203.0.113.8"); ok {
		t.Fatal("检查周期未到不应重新加载")
	}
	now = now.Add(time.Minute)
	if got, ok := l.Country(context.Background(), "203.0.113.8"); !ok || got != "us" {
		t.Fatalf("放上库文件后 = %q,%v，期望 us", got, ok)
	}
}
