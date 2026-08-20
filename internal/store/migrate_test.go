package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "modernc.org/sqlite"
)

// openRaw 打开一个不经过 initSchema 的裸连接,用于构造任意版本的库。
// :memory: 在 modernc 下按连接独立,须限制单连接以保证建表/查询落在同一库上。
func openRaw(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if dsn == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	v, err := readUserVersion(context.Background(), db)
	if err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

// TestOpenFreshStampsSchemaVersion 全新库:建 v1 表 + 打版本;再次打开幂等。
func TestOpenFreshStampsSchemaVersion(t *testing.T) {
	path := t.TempDir() + "/gw.db"

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if v := mustVersion(t, st.db); v != schemaVersion {
		t.Fatalf("schema version = %d, want %d", v, schemaVersion)
	}
	st.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	if v := mustVersion(t, st2.db); v != schemaVersion {
		t.Fatalf("reopen schema version = %d, want %d", v, schemaVersion)
	}
}

// TestOpenAdoptsPreVersioningDB 发布前旧开发库(user_version=0、业务表齐全)被认领,
// 版本打上且数据不破坏。
func TestOpenAdoptsPreVersioningDB(t *testing.T) {
	path := t.TempDir() + "/gw.db"
	raw := openRaw(t, "file:"+path)
	if _, err := raw.Exec(schema); err != nil {
		t.Fatalf("seed v1 schema: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO gw_settings (setting_key, value) VALUES ('gateway.id', 'legacy')`,
	); err != nil {
		t.Fatalf("seed legacy data: %v", err)
	}
	raw.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer st.Close()
	if v := mustVersion(t, st.db); v != schemaVersion {
		t.Fatalf("adopted version = %d, want %d", v, schemaVersion)
	}
	var v string
	if err := st.db.QueryRow(
		`SELECT value FROM gw_settings WHERE setting_key='gateway.id'`,
	).Scan(&v); err != nil || v != "legacy" {
		t.Fatalf("legacy data lost: value=%q err=%v", v, err)
	}
}

// TestOpenRejectsMismatchedPreVersioningDB 旧库业务表不齐全 → 拒绝并给出重建提示。
func TestOpenRejectsMismatchedPreVersioningDB(t *testing.T) {
	path := t.TempDir() + "/gw.db"
	raw := openRaw(t, "file:"+path)
	if _, err := raw.Exec(
		`CREATE TABLE gw_settings (setting_key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	); err != nil {
		t.Fatalf("seed partial table: %v", err)
	}
	raw.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("expected error opening mismatched pre-versioning db")
	}
}

// TestOpenRejectsNewerVersion 库版本比二进制新(如回滚二进制)→ 拒绝打开。
func TestOpenRejectsNewerVersion(t *testing.T) {
	path := t.TempDir() + "/gw.db"
	raw := openRaw(t, "file:"+path)
	if _, err := raw.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion+10)); err != nil {
		t.Fatalf("stamp newer version: %v", err)
	}
	raw.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("expected error opening newer-version db")
	}
}

// TestMigrateSchemaAppliesInOrder 用合成迁移验证机制:N-1 → N 逐级应用、按序、版本推进。
// 这是 v1.0.1 起真实迁移的"N-1 升级测试"范式。
func TestMigrateSchemaAppliesInOrder(t *testing.T) {
	db := openRaw(t, ":memory:")
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("base v1 schema: %v", err)
	}
	if err := setUserVersion(context.Background(), db, 1); err != nil {
		t.Fatalf("set v1: %v", err)
	}

	synthetic := []migration{
		{version: 2, name: "add column", up: func(tx *sql.Tx) error {
			_, err := tx.Exec(`ALTER TABLE gw_device ADD COLUMN foo TEXT NOT NULL DEFAULT ''`)
			return err
		}},
		{version: 3, name: "add table", up: func(tx *sql.Tx) error {
			_, err := tx.Exec(`CREATE TABLE gw_mig_test (id TEXT PRIMARY KEY)`)
			return err
		}},
	}
	if err := migrateSchema(context.Background(), db, 1, 3, synthetic); err != nil {
		t.Fatalf("migrate v1→v3: %v", err)
	}
	if v := mustVersion(t, db); v != 3 {
		t.Fatalf("version after migrate = %d, want 3", v)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('gw_device') WHERE name='foo'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("v2 column missing: n=%d err=%v", n, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='gw_mig_test'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("v3 table missing: n=%d err=%v", n, err)
	}
}

// TestMigrationTransactionRollback 迁移中途失败:DDL 与 user_version 一起回滚,
// 库仍停留在原版本(下次打开从已完成版本继续,不重复执行)。
func TestMigrationTransactionRollback(t *testing.T) {
	db := openRaw(t, ":memory:")
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("base v1 schema: %v", err)
	}
	if err := setUserVersion(context.Background(), db, 1); err != nil {
		t.Fatalf("set v1: %v", err)
	}
	failing := []migration{
		{version: 2, name: "bad", up: func(tx *sql.Tx) error {
			if _, err := tx.Exec(`CREATE TABLE gw_mig_bad (id TEXT PRIMARY KEY)`); err != nil {
				return err
			}
			return fmt.Errorf("boom")
		}},
	}
	if err := migrateSchema(context.Background(), db, 1, 2, failing); err == nil {
		t.Fatal("expected migration error")
	}
	if v := mustVersion(t, db); v != 1 {
		t.Fatalf("version after failed migration = %d, want 1 (rolled back)", v)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='gw_mig_bad'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("failed migration DDL not rolled back: n=%d err=%v", n, err)
	}
}
