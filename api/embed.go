// Package api 内嵌对外业务 API 契约 openapi.yaml（OpenAPI 3.1，docs/06 §4 的唯一事实源）。
// 解析与路由集合见 internal/control/apispec。
package api

import _ "embed"

// OpenAPI 是 openapi.yaml 的原始内容。
//
//go:embed openapi.yaml
var OpenAPI []byte
