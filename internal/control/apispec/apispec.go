// Package apispec 解析内嵌的 api/openapi.yaml，给出契约中的路由集合。
//
// 用途：internal/control/api 的契约测试断言“已注册路由集合 = OpenAPI 路径集合”（docs/06 §4）。
// 路径参数写法（{subscriptionId}）与 Go 1.22 net/http 路由模式一致，Route.Pattern() 可直接用于 ServeMux。
package apispec

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/go-gost/gost/api"
	"gopkg.in/yaml.v3"
)

// Route 是契约中的一个操作。
type Route struct {
	Method      string // 大写，如 GET
	Path        string // 如 /api/subscriptions/{subscriptionId}
	OperationID string
	Tags        []string
}

// Pattern 返回 net/http ServeMux 模式，如 "GET /api/me"。
func (r Route) Pattern() string { return r.Method + " " + r.Path }

// Admin 报告该路由是否属于管理端（/admin-api/）。
func (r Route) Admin() bool { return strings.HasPrefix(r.Path, "/admin-api/") }

var methods = []string{"get", "put", "post", "delete", "patch", "head", "options", "trace"}

var (
	once   sync.Once
	routes []Route
	err    error
)

// Routes 返回契约中的全部操作，按路径、方法排序。结果在进程内缓存，调用方不要修改。
func Routes() ([]Route, error) {
	once.Do(func() { routes, err = Parse(api.OpenAPI) })
	return routes, err
}

// Paths 返回 "METHOD /path" 形式的集合，供路由相等性测试比较。
func Paths() (map[string]Route, error) {
	rs, err := Routes()
	if err != nil {
		return nil, err
	}
	out := make(map[string]Route, len(rs))
	for _, r := range rs {
		out[r.Pattern()] = r
	}
	return out, nil
}

// Parse 从 OpenAPI 文档内容解析操作列表。
func Parse(doc []byte) ([]Route, error) {
	var d struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("apispec: 解析 openapi.yaml: %w", err)
	}
	var out []Route
	for path, item := range d.Paths {
		for _, m := range methods {
			n, ok := item[m]
			if !ok {
				continue
			}
			var op struct {
				OperationID string   `yaml:"operationId"`
				Tags        []string `yaml:"tags"`
			}
			if err := n.Decode(&op); err != nil {
				return nil, fmt.Errorf("apispec: %s %s: %w", strings.ToUpper(m), path, err)
			}
			out = append(out, Route{Method: strings.ToUpper(m), Path: path, OperationID: op.OperationID, Tags: op.Tags})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out, nil
}
