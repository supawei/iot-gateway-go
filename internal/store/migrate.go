package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// ---- 版本化 schema 迁移 ----
//
// 背景:发布前(开发期)按 development-conventions.md 约定不做结构迁移,直接改
// `CREATE TABLE`;发布后存在需要平滑升级的存量部署,必须引入版本化迁移。v1.0.0
// 冻结 schema 为版本 1,迁移机制在发布前搭好,首个真实迁移随 v1.0.1 的 schema
// 变更加入 migrations 并补 N-1 升级测试。

// schemaVersion 是当前二进制要求的 schema 版本。v1.0.0 冻结为 1(初始 schema 即版本 1)。
const schemaVersion = 1

// migration 是一个有序 schema 迁移:把库从 version-1 升级到 version。
// up 在事务内执行;事务提交前把 user_version 一并推进到目标版本,中途失败自动
// 回滚,下次打开从已完成的版本继续(不再重跑)。
type migration struct {
	version int
	name    string
	up      func(tx *sql.Tx) error
}

// migrations 按 version 升序排列。v1.0.0 无增量迁移(初始 schema 即版本 1);
// v1.0.1 起每次改表在此追加 {version: 下一号, name, up},并在 migrate_test.go
// 补对应 N-1 升级用例(见 docs/development-conventions.md §迁移)。
var migrations = []migration{}

// expectedV1Tables 是 v1.0.0 初始 schema 的全部业务表,用于"认领"发布前旧开发库
// (user_version=0)时校验结构一致,避免把残缺/历史形态库误标为 v1。
var expectedV1Tables = []string{
	"gw_connection", "gw_device", "gw_point", "gw_user", "gw_client",
	"gw_output", "gw_settings", "gw_alert_rule", "gw_alert",
}

// execer 是迁移所需的最小查询/执行能力,由 *sql.DB 与 *sql.Conn 共同满足。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// schemaDB 额外要求事务能力,由 *sql.DB 与 *sql.Conn 共同满足。
type schemaDB interface {
	execer
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// initSchema 在 Open 时保证库处于当前 schema 版本:全新建表 / 版本化迁移 / 认领旧库。
// 全程用单条连接执行:modernc 的 :memory: 库按连接独立,分到多条连接会互相看不到表。
func initSchema(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire schema connection: %w", err)
	}
	defer conn.Close()

	cur, err := readUserVersion(ctx, conn)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	switch {
	case cur == 0:
		// 空库(全新安装)或发布前旧开发库(从未打版本号)。
		return initOrAdopt(ctx, conn)
	case cur < schemaVersion:
		if err := migrateSchema(ctx, conn, cur, schemaVersion, migrations); err != nil {
			return err
		}
		return nil
	case cur == schemaVersion:
		return nil
	default:
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d); please upgrade the gateway binary", cur, schemaVersion)
	}
}

// initOrAdopt 处理 user_version=0 的库:
//   - 空库(全新安装):直接建 v1 初始 schema 并打版本;
//   - 发布前旧开发库:校验业务表齐全后"认领"为 v1 并打版本,不做破坏性重建。
func initOrAdopt(ctx context.Context, db execer) error {
	has, err := hasAnyTable(ctx, db)
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("init schema v%d: %w", schemaVersion, err)
		}
	} else {
		if err := verifyV1Tables(ctx, db); err != nil {
			return err
		}
		slog.Warn("detected pre-versioning database (user_version=0); adopting as schema v1",
			"schemaVersion", schemaVersion)
	}
	if err := setUserVersion(ctx, db, schemaVersion); err != nil {
		return fmt.Errorf("stamp schema version: %w", err)
	}
	return nil
}

func hasAnyTable(ctx context.Context, db execer) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`,
	).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func verifyV1Tables(ctx context.Context, db execer) error {
	for _, name := range expectedV1Tables {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name,
		).Scan(&n); err != nil {
			return fmt.Errorf("verify pre-versioning schema: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("pre-versioning database is missing table %q; it does not match schema v%d — rebuild by deleting the db file and restarting",
				name, schemaVersion)
		}
	}
	return nil
}

// migrateSchema 把库从 from 版逐级升到 to 版(from < version <= to 的迁移逐一执行)。
// list 独立传入便于测试用合成迁移验证机制,生产路径传全局 migrations。
func migrateSchema(ctx context.Context, db schemaDB, from, to int, list []migration) error {
	for _, m := range list {
		if m.version <= from {
			continue
		}
		if m.version > to {
			break
		}
		if err := applyOne(ctx, db, m); err != nil {
			return fmt.Errorf("migrate to v%d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

// applyOne 在单事务内执行一个迁移并把 user_version 推进到目标版本。
func applyOne(ctx context.Context, db schemaDB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := m.up(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	return tx.Commit()
}

func readUserVersion(ctx context.Context, db execer) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

func setUserVersion(ctx context.Context, db execer, v int) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v))
	return err
}
