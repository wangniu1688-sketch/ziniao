package pg

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// ---------- SQL 占位符数量自校验 ----------
//
// Postgres 对「SQL 里写了 $18、调用时只传 17 个实参」只回一句
// "mismatched param and argument count"：既不带是哪句 SQL，也不带少了几
// 个——M3 阶段 8 就是被它拖垮的：主提交与失败复位走同一条整行 UPDATE，
// 一次漏写让两条一起崩，环境悬在 running 上。
//
// 所以写语句在执行前先自己数一遍：出错时把「占位符几个 / 实参几个 / 哪句 SQL」
// 全部说清楚，并且**在打到数据库之前**失败（不会留下半个事务）。

var placeholderRe = regexp.MustCompile(`\$(\d{1,4})`)

// maxPlaceholder SQL 里出现过的最大占位符编号（没有占位符时为 0）。
//
// 只数 $N 这种 pgx 占位符：本文件的使用方都是固定 SQL 常量，不含字面量 $。
func maxPlaceholder(sql string) int {
	max := 0
	for _, m := range placeholderRe.FindAllStringSubmatch(sql, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > max {
			max = n
		}
	}
	return max
}

// execStmt 执行一条写语句，先校验占位符数量与实参数量一致。
//
// 校验放在这里而不是靠测试兜底：这类错误只在**特定分支**才走到（例如只有
// 启动失败复位才会碰到的那条语句），测试没覆盖的分支就是线上事故。
func execStmt(ctx context.Context, q querier, sql string, args ...any) (pgconn.CommandTag, error) {
	if n := maxPlaceholder(sql); n != len(args) {
		return pgconn.CommandTag{}, fmt.Errorf("pg: SQL 参数数量不匹配（占位符 %d 个 / 实参 %d 个）: %s",
			n, len(args), sqlDigest(sql))
	}
	return q.Exec(ctx, sql, args...)
}

// sqlDigest 把 SQL 压成一行并截断（错误信息要能一眼看全，又不能刷屏）。
func sqlDigest(sql string) string {
	s := strings.Join(strings.Fields(sql), " ")
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
