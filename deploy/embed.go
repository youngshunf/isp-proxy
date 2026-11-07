// Package deploy 存放单节点部署示例（systemd 单元、反向代理、备份脚本、配置与开发种子数据），全部为占位值。
// 本文件只把开发种子数据内嵌给 `isp-control seed-dev` 使用。
package deploy

import _ "embed"

// SeedSQL 是 seed.example.sql 的内容。
//
//go:embed seed.example.sql
var SeedSQL string
