# 开发约定

> **更新**:2026-08-19

## 尚未发布:配置 / 数据库变更不做迁移

**状态**:生效中(项目开发阶段,发布前清理)

项目目前处于**开发阶段,尚未对外发布**。因此:

- `config.yaml` 的结构变更、SQLite 表结构变更,一律**直接修改**,不提供向后兼容迁移(不做旧配置自动导入、不做 schema 版本升级)。
- 遇到结构变化时,旧配置文件 / 旧 `.db` 文件由使用者自行重建(删除后重启即可重新生成)。
- **发布前清理(2026-08-19)**:已移除 `internal/store` / `internal/backfill` 中"开发期结构演进"的自动补列/改名 shim(表重命名、`ALTER TABLE ADD COLUMN`、`RENAME COLUMN`)。新装库直接按最终 `CREATE TABLE` schema 建表;旧 dev 库(含 `user`/`settings(key)`/`connection` 等历史形态)不再自动升级,按上文约定重建。

理由:开发期没有存量部署,迁移代码是纯负担,且会模糊当前真实结构;发布后这些 shim 即死代码。

**作废条件**:一旦项目对外发布、存在需要平滑升级的存量部署,应引入版本化迁移机制(如 SQLite `PRAGMA user_version` + 迁移脚本、配置结构版本号),届时本条约定作废并移入「暂缓清单」。

## 相关决策

- 北向输出配置从 `config.yaml` 迁到 SQLite 时即按此约定执行:直接删除 `config.yaml` 里的输出段,不做旧 yaml 一次性迁移。见 [northbound-output-config.md](northbound-output-config.md)。
