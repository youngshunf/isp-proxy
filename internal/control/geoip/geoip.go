// Package geoip 按出口 IP 解析国家代码，供探测结果判定国家是否符合套餐与上游声明（kernel.GeoLookup）。
//
// 解析顺序：先查手工映射（core.exit_countries，可写单个 IP 或网段，最长前缀优先），
// 查不到再查 MaxMind DB 格式的国家库（core.geoip_db，DB-IP Country Lite 或 GeoLite2 Country）。
// 手工映射用来纠正归属地库判错的个别 IP，或在没有库文件时兜底。
package geoip

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

var ccRe = regexp.MustCompile(`^[a-z]{2}$`)

// Overrides 是手工“IP 或网段 → 国家代码”映射，最长前缀优先。
type Overrides struct {
	rules []rule // 按前缀长度从长到短
}

type rule struct {
	p  netip.Prefix
	cc string
}

// ParseOverrides 校验并规范化手工映射：键是 IP（IPv4/IPv6，任意写法）或 CIDR 网段，值是两位国家代码（大小写不限）。
func ParseOverrides(m map[string]string) (*Overrides, error) {
	o := &Overrides{}
	for k, v := range m {
		key := strings.TrimSpace(k)
		cc := strings.ToLower(strings.TrimSpace(v))
		if !ccRe.MatchString(cc) {
			return nil, fmt.Errorf("exit_countries %q：国家代码 %q 不是两位字母", k, v)
		}
		var p netip.Prefix
		if strings.Contains(key, "/") {
			pp, err := netip.ParsePrefix(key)
			if err != nil {
				return nil, fmt.Errorf("exit_countries %q：不是合法的网段", k)
			}
			p = pp.Masked()
			if p.Addr().Is4In6() {
				return nil, fmt.Errorf("exit_countries %q：IPv4 网段请直接写 IPv4 形式", k)
			}
		} else {
			a, err := netip.ParseAddr(key)
			if err != nil {
				return nil, fmt.Errorf("exit_countries %q：不是合法的 IP", k)
			}
			a = a.Unmap().WithZone("")
			p = netip.PrefixFrom(a, a.BitLen())
		}
		o.rules = append(o.rules, rule{p: p, cc: cc})
	}
	sort.SliceStable(o.rules, func(i, j int) bool { return o.rules[i].p.Bits() > o.rules[j].p.Bits() })
	return o, nil
}

// Len 返回映射条数。
func (o *Overrides) Len() int {
	if o == nil {
		return 0
	}
	return len(o.rules)
}

func (o *Overrides) lookup(a netip.Addr) (string, bool) {
	if o == nil {
		return "", false
	}
	for _, r := range o.rules {
		if r.p.Contains(a) {
			return r.cc, true
		}
	}
	return "", false
}

// DB 是按文件路径打开的国家库。文件被替换（mtime 或大小变化）后自动重新加载，
// 文件暂缺时查询一律返回未知、之后出现再加载，所以定时更新库文件不需要重启控制面。
type DB struct {
	path   string
	log    *slog.Logger
	every  time.Duration
	now    func() time.Time
	mu     sync.RWMutex
	r      *maxminddb.Reader
	sig    string // 已加载文件的 mtime+大小
	nextAt time.Time
	warned bool
}

// OpenDB 打开国家库；文件不存在或损坏时只记日志，不返回错误（手工映射仍然生效）。
func OpenDB(path string, log *slog.Logger) *DB {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	d := &DB{path: path, log: log, every: time.Minute, now: time.Now}
	d.reload()
	return d
}

// Close 释放库文件。
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.r == nil {
		return nil
	}
	err := d.r.Close()
	d.r = nil
	return err
}

// Loaded 报告库文件是否已加载。
func (d *DB) Loaded() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.r != nil
}

func (d *DB) maybeReload() {
	d.mu.RLock()
	due := !d.now().Before(d.nextAt)
	d.mu.RUnlock()
	if due {
		d.reload()
	}
}

func (d *DB) reload() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nextAt = d.now().Add(d.every)
	st, err := os.Stat(d.path)
	if err != nil {
		if !d.warned {
			d.log.Warn("IP 归属地库不可用，只按 exit_countries 手工映射判定国家", "geoip_db", d.path, "err", err)
			d.warned = true
		}
		return
	}
	sig := fmt.Sprintf("%d/%d", st.ModTime().UnixNano(), st.Size())
	if sig == d.sig && d.r != nil {
		return
	}
	r, err := maxminddb.Open(d.path)
	if err != nil {
		d.log.Error("IP 归属地库打开失败，沿用已加载的版本", "geoip_db", d.path, "err", err)
		d.sig = sig // 同一份坏文件不反复尝试
		return
	}
	if d.r != nil {
		_ = d.r.Close()
	}
	d.r, d.sig, d.warned = r, sig, false
	d.log.Info("已加载 IP 归属地库", "geoip_db", d.path, "type", r.Metadata.DatabaseType,
		"built", time.Unix(int64(r.Metadata.BuildEpoch), 0).UTC().Format(time.DateOnly))
}

type record struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	RegisteredCountry struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"registered_country"`
}

func (d *DB) lookup(a netip.Addr) (string, bool) {
	if d == nil {
		return "", false
	}
	d.maybeReload()
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.r == nil {
		return "", false
	}
	var rec record
	if err := d.r.Lookup(a).Decode(&rec); err != nil {
		return "", false
	}
	cc := rec.Country.ISOCode
	if cc == "" { // 任播等地址只有注册国家
		cc = rec.RegisteredCountry.ISOCode
	}
	cc = strings.ToLower(cc)
	return cc, ccRe.MatchString(cc)
}

// Lookup 组合手工映射与国家库，实现 kernel.GeoLookup。两者都可为空。
type Lookup struct {
	Overrides *Overrides
	DB        *DB
}

// Country 实现 kernel.GeoLookup：ip 任意写法均可（IPv4 映射的 IPv6 按 IPv4 处理）。
func (l Lookup) Country(_ context.Context, ip string) (string, bool) {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return "", false
	}
	a = a.Unmap().WithZone("")
	if cc, ok := l.Overrides.lookup(a); ok {
		return cc, true
	}
	return l.DB.lookup(a)
}
