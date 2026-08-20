# 开发约定

> **更新**:2026-08-20

## 发布前:配置 / 数据库变更不做迁移(机制已就位)

**状态**:生效中(项目开发阶段)。v1.0.0 冻结 schema = 版本 1;迁移**框架已搭好**,但 `migrations` 为空——发布前仍不做任何结构迁移。

- `config.yaml` 的结构变更、SQLite 表结构变更,发布前一律**直接修改**,不提供向后兼容迁移;旧配置文件 / 旧 `.db` 由使用者自行重建(删除后重启重新生成)。
- **发布前清理(2026-08-19)**:已移除 `internal/store` / `internal/backfill` 中"开发期结构演进"的自动补列/改名 shim。
- **迁移机制就位(2026-08-20)**:`internal/store/migrate.go` 引入版本化迁移——`PRAGMA user_version` 记录版本 + 有序 `migrations` + 事务内推进版本(中途失败自动回滚)+ 全新/认领/升级/拒绝四路径 + N-1 升级测试范式(`migrate_test.go` 用合成迁移验证)。`Open()` 对 `user_version=0` 的发布前旧库(业务表齐全)自动**认领**为 v1 并打版本,不破坏数据;表结构不匹配则明确报错提示重建。

**发布后(首个 schema 变更起)**:每次改表遵循:

1. 在 `migrations` 追加 `{version: schemaVersion+1, name, up}` 并把 `schemaVersion` 加 1;
2. 在 `migrate_test.go` 补该迁移的 **N-1 升级用例**(从 vN 建库 → 跑迁移 → 断言结构与版本);
3. 迁移 `up` 在事务内执行,`IF NOT EXISTS`/`ADD COLUMN` 等要可重入(配合事务回滚保证安全)。

## 相关决策

- 北向输出配置从 `config.yaml` 迁到 SQLite 时即按此约定执行:直接删除 `config.yaml` 里的输出段,不做旧 yaml 一次性迁移。见 [northbound-output-config.md](northbound-output-config.md)。
