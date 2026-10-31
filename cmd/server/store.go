package main

import (
	"context"
	"fmt"
	"log/slog"

	"ziniao/platform/config"
	"ziniao/platform/migrate"
	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/store/pg"
)

// openStore 按配置创建存储驱动；postgres 模式可选执行迁移。
func openStore(ctx context.Context, cfg *config.Config, runMigrate bool, log *slog.Logger) (store.Store, error) {
	switch cfg.StoreDriver {
	case "postgres":
		st, err := pg.New(ctx, cfg.PostgresDSN)
		if err != nil {
			return nil, err
		}
		if runMigrate {
			applied, err := migrate.Up(ctx, st.Pool())
			if err != nil {
				return nil, fmt.Errorf("数据库迁移失败: %w", err)
			}
			log.Info("迁移完成", "versions", applied)
		}
		return st, nil
	case "memory", "":
		return mem.New(), nil
	default:
		return nil, fmt.Errorf("未知存储驱动: %s", cfg.StoreDriver)
	}
}
