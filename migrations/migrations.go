// Package migrations 内嵌 SQL 迁移文件，使二进制自包含（无需外部文件即可迁移）。
package migrations

import "embed"

// FS 内嵌的迁移脚本：{version}.up.sql / {version}.down.sql
//
//go:embed *.sql
var FS embed.FS
