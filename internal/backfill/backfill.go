// Package backfill 提供北向输出的断网本地补传(离线缓存)持久化队列。
//
// 设计:采集数据在"无法即时送出"(断连 / 上送失败 / 缓冲满)时,由输出或
// Manager 经 Save 落库到 SQLite 表 gw_backfill_queue;恢复后由 Manager 按序
// Peek → 重放 → Ack(确认删除)。队列按 output_id 隔离(每输出独立),有全局
// 上限,超出时淘汰最旧数据,保证磁盘有界。详见 docs/offline-backfill-design.md。
package backfill

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"iot-gateway-go/internal/model"
)

// DefaultMax 是单输出补传队列的默认上限:超过后按 id 淘汰最旧数据。
// 约 10 万条 × ~500B ≈ 50MB 量级,避免断连超长期间磁盘无限增长。
const DefaultMax = 100_000

const schema = `
CREATE TABLE IF NOT EXISTS gw_backfill_queue (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    output_id  TEXT    NOT NULL,
    payload    TEXT    NOT NULL,
    created_at TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_gw_backfill_output ON gw_backfill_queue (output_id, id);`

// Item 是一条待补传记录:ID 用于确认删除,DP 为还原的采集数据点。
type Item struct {
	ID int64
	DP model.DataPoint
}

// Store 是补传队列的持久化存储,底层复用网关的 SQLite(WAL)。
type Store struct {
	db  *sql.DB
	max int
}

// New 构造补传存储并确保表结构存在。max<=0 时用 DefaultMax。
func New(db *sql.DB, max int) (*Store, error) {
	if max <= 0 {
		max = DefaultMax
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("backfill schema: %w", err)
	}
	return &Store{db: db, max: max}, nil
}

// Save 把一批数据点入队;超出上限时按 id 淘汰最旧数据(保最新),并告警。
// 单批事务,保证要么整批成功要么整批失败。
func (s *Store) Save(outputID string, dps []model.DataPoint) error {
	if len(dps) == 0 {
		return nil
	}
	// created_at 统一存 RFC3339Nano 文本(与 gw_client/gw_alert_rule/gw_alert 一致);
	// 队列顺序以自增 id 为准,created_at 仅留档。
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("backfill begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO gw_backfill_queue (output_id, payload, created_at) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("backfill prepare: %w", err)
	}
	defer stmt.Close()

	for _, dp := range dps {
		data, err := json.Marshal(dp)
		if err != nil {
			// 单个点序列化失败(极端:值类型不可 JSON 化)跳过该点,不影响其余。
			slog.Warn("backfill marshal datapoint failed", "device", dp.DeviceID, "point", dp.Point, "err", err)
			continue
		}
		if _, err := stmt.Exec(outputID, string(data), now); err != nil {
			return fmt.Errorf("backfill insert: %w", err)
		}
	}

	if err := s.evictOver(tx, outputID); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("backfill commit: %w", err)
	}
	return nil
}

// evictOver 在事务内淘汰该 output 超出上限的最旧数据(按 id 升序删)。
func (s *Store) evictOver(tx *sql.Tx, outputID string) error {
	var total int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM gw_backfill_queue WHERE output_id = ?`, outputID).Scan(&total); err != nil {
		return fmt.Errorf("backfill count: %w", err)
	}
	if total <= s.max {
		return nil
	}
	over := total - s.max
	res, err := tx.Exec(
		`DELETE FROM gw_backfill_queue WHERE id IN (
			SELECT id FROM gw_backfill_queue WHERE output_id = ? ORDER BY id LIMIT ?)`,
		outputID, over,
	)
	if err != nil {
		return fmt.Errorf("backfill evict: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		slog.Warn("backfill queue over cap, evicted oldest",
			"output", outputID, "evicted", n, "cap", s.max)
	}
	return nil
}

// CountByOutput 返回指定输出的待补传条数。
func (s *Store) CountByOutput(outputID string) (int, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM gw_backfill_queue WHERE output_id = ?`, outputID).Scan(&n); err != nil {
		return 0, fmt.Errorf("backfill count: %w", err)
	}
	return n, nil
}

// TotalCount 返回全部输出的待补传总条数(观测用)。
func (s *Store) TotalCount() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM gw_backfill_queue`).Scan(&n); err != nil {
		return 0, fmt.Errorf("backfill total count: %w", err)
	}
	return n, nil
}

// Peek 取该输出最旧的一批待补传记录(按 id 升序),最多 limit 条。
// 返回的 Item.DP 已还原为采集数据点。
func (s *Store) Peek(outputID string, limit int) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT id, payload FROM gw_backfill_queue WHERE output_id = ? ORDER BY id LIMIT ?`,
		outputID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("backfill peek: %w", err)
	}
	defer rows.Close()

	items := make([]Item, 0, limit)
	for rows.Next() {
		var it Item
		var payload string
		if err := rows.Scan(&it.ID, &payload); err != nil {
			return nil, fmt.Errorf("backfill peek scan: %w", err)
		}
		if err := json.Unmarshal([]byte(payload), &it.DP); err != nil {
			// 单条损坏:跳过不返回,避免阻塞整队重放。
			slog.Warn("backfill payload corrupt, skip", "output", outputID, "id", it.ID, "err", err)
			continue
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// Ack 确认一批记录已成功投递,从队列删除。不存在的 id 静默忽略(幂等)。
func (s *Store) Ack(outputID string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	// 分批删除,避免单个超长 IN 子句;按 output_id 双重限定防串队。
	for i := 0; i < len(ids); i += 500 {
		end := i + 500
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[i:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, outputID)
		placeholders := ""
		for j, id := range chunk {
			if j > 0 {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, id)
		}
		if _, err := s.db.Exec(
			`DELETE FROM gw_backfill_queue WHERE output_id = ? AND id IN (`+placeholders+`)`,
			args...,
		); err != nil {
			return fmt.Errorf("backfill ack: %w", err)
		}
	}
	return nil
}

// DropOutput 清空指定输出的全部待补传记录(输出配置被删除时调用)。
func (s *Store) DropOutput(outputID string) error {
	if _, err := s.db.Exec(
		`DELETE FROM gw_backfill_queue WHERE output_id = ?`, outputID); err != nil {
		return fmt.Errorf("backfill drop output: %w", err)
	}
	return nil
}
