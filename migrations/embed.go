// Package migrations 内嵌控制面数据库迁移文件，供 internal/control/store 按序执行。
//
// 文件命名为 NNNN_描述.sql，只前进：已发布的文件不得修改（迁移器会校验校验和），
// 改动一律写成新文件。
package migrations

import "embed"

// FS 包含本目录下全部 *.sql 迁移文件。
//
//go:embed *.sql
var FS embed.FS
