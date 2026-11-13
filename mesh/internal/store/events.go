package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"mesh/internal/contract"
)

// 事件日志（§4.3 / §9.1）。
//
// 唯一约束 (run_id, seq)：seq 在单个 Run 内单调递增，由事件生产方分配。
// 重复投递同一个 (run_id, seq) 一律幂等丢弃——终态事件（final/error）重发很常见，
// 不能因此写出两条记录。
//
// 为什么同时存列和 payload_json：列（run_id/seq/ts/type）用于索引与游标查询，
// payload_json 存完整 contract.Event，读回时原样还原，避免列与结构体二次映射出错。
// 两者不一致由写入方保证（只从 contract.Event 生成，不手写列）。
//
// 注意：output 类的流式文本事件只用于展示，绝不能用它触发唤醒（§5.6）——
// 是否唤醒由上层按 contract.EventKind 判断，存储层不做语义决策。

// ErrEventSeqInvalid 表示 seq 非法（必须 ≥ 1）。
var ErrEventSeqInvalid = errors.New("store: 事件 seq 必须 ≥ 1")

// AppendEvent 追加一条事件（编排层 orchestrator.Store 接口的签名）。
//
// 幂等语义：同一个 (run_id, seq) 重复写入时静默成功返回 nil，不报错——
// 去重是存储层的责任，编排层不应为重复终态事件而中断（§4.3）。
//
// 用 ON CONFLICT DO NOTHING 而不是捕获驱动错误码：错误码是驱动细节，
// 存储层不应把 modernc.org/sqlite 的错误码泄漏给上层。
func (s *Store) AppendEvent(ctx context.Context, ev contract.Event) error {
	if ev.RunID == "" {
		return errors.New("store: event 缺少 run_id")
	}
	if ev.Seq < 1 {
		return fmt.Errorf("%w (run=%s, seq=%d)", ErrEventSeqInvalid, ev.RunID, ev.Seq)
	}

	eventID := ev.EventID
	if eventID == "" {
		// 派生一个确定性 id，保证上层去重键永远非空（重复写入同一 seq 得到同一个 id）。
		eventID = fmt.Sprintf("%s:%d", ev.RunID, ev.Seq)
	}
	ts := ev.TS
	if ts.IsZero() {
		ts = time.Now()
	}
	// 写入前把补齐后的字段回填到事件对象，保证 payload 与列完全一致。
	stored := ev
	stored.EventID = eventID
	stored.TS = ts
	if stored.SchemaVersion == 0 {
		stored.SchemaVersion = contract.SchemaVersion
	}

	payload, err := json.Marshal(&stored)
	if err != nil {
		return fmt.Errorf("store: 序列化事件失败: %w", err)
	}

	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO events (id, run_id, seq, type, payload_json, ts)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT (run_id, seq) DO NOTHING`,
			eventID, stored.RunID, stored.Seq, string(stored.Kind), string(payload), ts.UnixMilli(),
		); err != nil {
			return fmt.Errorf("store: 写入事件失败(run=%s, seq=%d): %w", stored.RunID, stored.Seq, err)
		}
		return nil
	})
}

// AppendEvents 在一个事务里批量追加事件，用于流式 output 事件的攒批写入
// （逐条提交在 WAL 下也安全，但攒批能显著降低 fsync 次数）。
// 返回实际新写入的条数；已存在的 (run_id, seq) 静默跳过。
func (s *Store) AppendEvents(ctx context.Context, evs []contract.Event) (int, error) {
	if len(evs) == 0 {
		return 0, nil
	}
	// 先整体校验，避免写了一半才发现 seq 非法。
	for i := range evs {
		if evs[i].RunID == "" {
			return 0, fmt.Errorf("store: 第 %d 条事件缺少 run_id", i)
		}
		if evs[i].Seq < 1 {
			return 0, fmt.Errorf("%w (run=%s, seq=%d)", ErrEventSeqInvalid, evs[i].RunID, evs[i].Seq)
		}
	}

	inserted := 0
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO events (id, run_id, seq, type, payload_json, ts)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT (run_id, seq) DO NOTHING`)
		if err != nil {
			return fmt.Errorf("store: 预编译事件写入失败: %w", err)
		}
		defer func() { _ = stmt.Close() }()

		for i := range evs {
			ev := evs[i]
			if ev.EventID == "" {
				ev.EventID = fmt.Sprintf("%s:%d", ev.RunID, ev.Seq)
			}
			if ev.TS.IsZero() {
				ev.TS = time.Now()
			}
			if ev.SchemaVersion == 0 {
				ev.SchemaVersion = contract.SchemaVersion
			}
			payload, err := json.Marshal(&ev)
			if err != nil {
				return fmt.Errorf("store: 序列化事件失败(run=%s, seq=%d): %w", ev.RunID, ev.Seq, err)
			}
			res, err := stmt.ExecContext(ctx,
				ev.EventID, ev.RunID, ev.Seq, string(ev.Kind), string(payload), ev.TS.UnixMilli(),
			)
			if err != nil {
				return fmt.Errorf("store: 写入事件失败(run=%s, seq=%d): %w", ev.RunID, ev.Seq, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("store: 读取 RowsAffected 失败: %w", err)
			}
			if n == 1 {
				inserted++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

// EventCursor 是事件补读游标：从某个 Run 的 afterSeq 之后继续读。
// 上层重连时带上自己最后处理过的 (RunID, Seq)，存储层只返回更大的 seq，
// 再配合 EventID 去重，就能做到断线补齐且不重复渲染（§4.3）。
type EventCursor struct {
	RunID    string
	AfterSeq int64
}

// EventsSince 按游标补读事件，按 seq 升序返回。
//
// limit 必须 > 0；返回的最后一个事件的 seq 即新的游标位置（没有新事件时游标不动）。
func (s *Store) EventsSince(ctx context.Context, cur EventCursor, limit int) ([]contract.Event, error) {
	if cur.RunID == "" {
		return nil, errors.New("store: 事件游标缺少 run_id")
	}
	if limit <= 0 {
		return nil, errors.New("store: EventsSince 的 limit 必须 > 0")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT payload_json FROM events
		  WHERE run_id = ? AND seq > ?
		  ORDER BY seq
		  LIMIT ?`,
		cur.RunID, cur.AfterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 补读事件失败(run=%s): %w", cur.RunID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []contract.Event
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取事件行失败: %w", err)
		}
		var ev contract.Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return nil, fmt.Errorf("store: 解析事件 payload 失败: %w", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历事件失败: %w", err)
	}
	return out, nil
}

// MaxEventSeq 返回某个 Run 已持久化的最大 seq，0 表示还没有事件。
// 进程重启后用它恢复 seq 分配位置，保证 seq 单调不回头。
func (s *Store) MaxEventSeq(ctx context.Context, runID string) (int64, error) {
	var seq sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM events WHERE run_id = ?`, runID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("store: 查询最大 seq 失败(run=%s): %w", runID, err)
	}
	return seq.Int64, nil
}

// EventsByRun 读取某个 Run 的全部事件（按 seq 升序），用于导出与故障复盘。
// 正常补读请用 EventsSince，不要用本方法拉全量。
func (s *Store) EventsByRun(ctx context.Context, runID string) ([]contract.Event, error) {
	return s.EventsSince(ctx, EventCursor{RunID: runID}, 1<<20)
}

// EventRow 是事件的时间线投影，用于房间/Work 维度的展示，不含完整 payload。
type EventRow struct {
	EventID string
	RunID   string
	Seq     int64
	Kind    string
	TS      time.Time
	Text    string
}

// EventsByWork 按时间顺序读取某个 Work 下所有 Run 的事件（跨 Run 汇总时间线）。
// sinceMs 为 0 表示从头读；返回条数受 limit 限制。
func (s *Store) EventsByWork(ctx context.Context, workID string, sinceMs int64, limit int) ([]EventRow, error) {
	if workID == "" {
		return nil, errors.New("store: EventsByWork 需要 work_id")
	}
	if limit <= 0 {
		return nil, errors.New("store: EventsByWork 的 limit 必须 > 0")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.id, e.run_id, e.seq, e.type, e.ts, e.payload_json
		   FROM events e
		   JOIN runs r ON r.id = e.run_id
		  WHERE r.work_id = ? AND e.ts >= ?
		  ORDER BY e.ts, e.run_id, e.seq
		  LIMIT ?`,
		workID, sinceMs, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 Work 事件失败(work=%s): %w", workID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []EventRow
	for rows.Next() {
		var (
			row     EventRow
			tsMs    int64
			payload string
		)
		if err := rows.Scan(&row.EventID, &row.RunID, &row.Seq, &row.Kind, &tsMs, &payload); err != nil {
			return nil, fmt.Errorf("store: 读取 Work 事件行失败: %w", err)
		}
		row.TS = time.UnixMilli(tsMs).UTC()
		// Text 只取展示用文本；payload 解析失败不应让整个时间线不可读。
		var ev contract.Event
		if err := json.Unmarshal([]byte(payload), &ev); err == nil {
			row.Text = ev.Text
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 Work 事件失败: %w", err)
	}
	return out, nil
}

// EventsAfterRowid 按全局单调游标补读事件，供本机 UI 断线补齐（§5.6 / web 层 EventStream）。
//
// 为什么用 rowid 而不是 (run_id, seq)：单聊天框的全局时间线需要一个全序游标，
// (run_id, seq) 只在单个 Run 内单调。SQLite 的隐式 rowid 随插入单调递增且唯一，
// 断线重连时"rowid > cursor"天然不重不漏。
//
// 返回 next = 最后一条事件的 rowid（没有新事件时原样返回入参），
// 调用方下次把它传回来即可推进游标。
func (s *Store) EventsAfterRowid(ctx context.Context, rowid int64, limit int) ([]contract.Event, int64, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, payload_json FROM events WHERE rowid > ? ORDER BY rowid LIMIT ?`,
		rowid, limit)
	if err != nil {
		return nil, rowid, fmt.Errorf("store: 按游标查询事件失败(cursor=%d): %w", rowid, err)
	}
	defer func() { _ = rows.Close() }()

	var out []contract.Event
	next := rowid
	for rows.Next() {
		var (
			id      int64
			payload string
		)
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, next, fmt.Errorf("store: 读取事件行失败: %w", err)
		}
		var ev contract.Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			// 单条 payload 损坏不让整条时间线不可读：跳过并继续。
			continue
		}
		out = append(out, ev)
		next = id
	}
	if err := rows.Err(); err != nil {
		return nil, next, fmt.Errorf("store: 遍历事件失败: %w", err)
	}
	return out, next, nil
}
