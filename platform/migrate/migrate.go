// Package migrate 极简迁移器：按版本号顺序执行 up 脚本，支持 down 回滚。
// 每条迁移必须带 schema 版本号且可回滚（交付物规范）。
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"ziniao/migrations"
)

// Up 执行全部未应用的 up 脚本。
func Up(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	versions, err := pending(ctx, pool)
	if err != nil {
		return nil, err
	}
	applied := []string{}
	for _, v := range versions {
		sql, err := read(v + ".up.sql")
		if err != nil {
			return applied, err
		}
		if err := execScript(ctx, pool, v, sql); err != nil {
			return applied, err
		}
		applied = append(applied, v)
	}
	return applied, nil
}

// Down 按版本号倒序回滚。
func Down(ctx context.Context, pool *pgxpool.Pool, steps int) ([]string, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	done := []string{}
	for rows.Next() && (steps <= 0 || len(done) < steps) {
		var v string
		if err := rows.Scan(&v); err != nil {
			return done, err
		}
		sql, err := read(v + ".down.sql")
		if err != nil {
			return done, err
		}
		if err := execScript(ctx, pool, v, sql); err != nil {
			return done, err
		}
		if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, v); err != nil {
			return done, err
		}
		done = append(done, v)
	}
	return done, rows.Err()
}

func execScript(ctx context.Context, pool *pgxpool.Pool, version, sql string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("迁移 %s 执行失败: %w", version, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations(version) VALUES($1) ON CONFLICT(version) DO NOTHING`, version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func pending(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return nil, err
	}
	all := []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		all = append(all, strings.TrimSuffix(e.Name(), ".up.sql"))
	}
	sort.Strings(all)
	out := []string{}
	for _, v := range all {
		var exists string
		err := pool.QueryRow(ctx, `SELECT version FROM schema_migrations WHERE version = $1`, v).Scan(&exists)
		if err == nil && exists != "" {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

func read(name string) (string, error) {
	b, err := fs.ReadFile(migrations.FS, path.Clean(name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("迁移文件缺失: %s", name)
		}
		return "", err
	}
	return string(b), nil
}
