package store

// 编排层对接（P0b-5 组装阶段）。
//
// 本文件让 *Store 直接满足 orchestrator.Store 接口（internal/orchestrator/store.go，
// 13 个方法），app 组装层把 *store.Store 传给 orchestrator.New 即可，中间不需要适配器。
//
// 设计约束：
//   - 全部写入走 s.Write 串行队列；全部查询参数化，不拼接用户输入。
//   - 查不到一律原样返回 orchestrator.ErrNotFound，编排层用 errors.Is 判断。
//   - 复杂结构（Work / RoleAssignment）按 events.payload_json 的同一策略存 JSON，
//     列只承担索引与查询职责，避免两套字段映射互相漂移。
//   - reviews.assignment_version 在写入时从 works 行同步读取（§9.3：审查绑定
//     当时的分工版本），contract.Review 本身不带该字段。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"mesh/internal/contract"
	"mesh/internal/orchestrator"
)

// ---------------- Work ----------------

// SaveWork 保存或更新一个 Work（编排视角 upsert）。
func (s *Store) SaveWork(ctx context.Context, w contract.Work) error {
	if w.ID == "" || w.RoomID == "" {
		return errors.New("store: work 缺少 id 或 room_id")
	}
	payload, err := json.Marshal(w)
	if err != nil {
		return fmt.Errorf("store: 序列化 work 失败: %w", err)
	}

	// 从结构体派生列值；派生不出来的列（root_message_id / resume_phase / ended_at）存 NULL。
	latestArtifact := 0
	for _, a := range w.Artifacts {
		if a.Version > latestArtifact {
			latestArtifact = a.Version
		}
	}
	if w.Final != nil && w.Final.Version > latestArtifact {
		latestArtifact = w.Final.Version
	}
	var budgetJSON any
	if w.Budget != nil {
		b, err := json.Marshal(w.Budget)
		if err != nil {
			return fmt.Errorf("store: 序列化 work 预算失败: %w", err)
		}
		budgetJSON = string(b)
	}
	var resultRef any
	if w.Final != nil {
		resultRef = w.Final.Ref
	}
	var endedAt any
	if w.State == contract.WorkCompleted || w.State == contract.WorkFailed || w.State == contract.WorkCancelled {
		endedAt = w.UpdatedAt.UnixMilli()
	}

	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO works
				(id, room_id, thread_id, root_message_id, goal, executor_id, reviewer_id,
				 assignment_version, status, resume_phase, artifact_version, revision_count,
				 budget_json, result_ref, created_at, ended_at, work_json)
			 VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET
				goal = excluded.goal,
				executor_id = excluded.executor_id,
				reviewer_id = excluded.reviewer_id,
				assignment_version = excluded.assignment_version,
				status = excluded.status,
				artifact_version = excluded.artifact_version,
				revision_count = excluded.revision_count,
				budget_json = excluded.budget_json,
				result_ref = excluded.result_ref,
				ended_at = excluded.ended_at,
				work_json = excluded.work_json`,
			w.ID, w.RoomID, w.ThreadID, w.Goal, nullableText(w.Assign.Executor), nullableText(w.Assign.Reviewer),
			w.Assign.Version, string(w.State), latestArtifact, w.ReviewRounds,
			budgetJSON, resultRef, w.CreatedAt.UnixMilli(), endedAt, string(payload),
		)
		if err != nil {
			return fmt.Errorf("store: 保存 work %s 失败: %w", w.ID, err)
		}
		// works.room_id 有外键约束；room 不存在时 SQLite 会报 FK 错误，
		// 这里不改写错误内容——错误链里已有原始信息，上层看得懂。
		return nil
	})
}

// GetWork 按 id 读取 Work。查不到返回 orchestrator.ErrNotFound。
func (s *Store) GetWork(ctx context.Context, workID string) (contract.Work, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT work_json FROM works WHERE id = ?`, workID,
	).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return contract.Work{}, orchestrator.ErrNotFound
	}
	if err != nil {
		return contract.Work{}, fmt.Errorf("store: 读取 work %s 失败: %w", workID, err)
	}
	return decodeWork(payload)
}

// ListWorks 按房间与状态列表。states 为空表示不过滤状态。
// 结果按创建时间升序（同一房间内先建的事件在前，与聊天时间线一致）。
func (s *Store) ListWorks(ctx context.Context, roomID string, states []contract.WorkState) ([]contract.Work, error) {
	query := `SELECT work_json FROM works WHERE room_id = ?`
	args := []any{roomID}
	if len(states) > 0 {
		placeholders := make([]string, len(states))
		for i, st := range states {
			placeholders[i] = "?"
			args = append(args, string(st))
		}
		query += ` AND status IN (` + strings.Join(placeholders, ", ") + `)`
	}
	query += ` ORDER BY created_at, id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 列出 works 失败(room=%s): %w", roomID, err)
	}
	defer rows.Close()

	var out []contract.Work
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取 work 行失败: %w", err)
		}
		w, err := decodeWork(payload)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func decodeWork(payload []byte) (contract.Work, error) {
	var w contract.Work
	if len(payload) == 0 {
		return w, errors.New("store: work_json 为空（旧版本数据，无法还原 Work）")
	}
	if err := json.Unmarshal(payload, &w); err != nil {
		return contract.Work{}, fmt.Errorf("store: 解析 work_json 失败: %w", err)
	}
	return w, nil
}

// ---------------- Message ----------------

// SaveMessage 保存一条消息（编排视角，不建投递行）。
//
// 与 CreateMessage 的分工：CreateMessage 是投递视角（消息+投递行同事务，提交后才允许
// 唤醒 worker）；本方法是编排层回写协作消息（bot 之间 handoff、系统 notice 等）用的，
// 这些消息要不要投递、投给谁，由编排层自己决定后再调 CreateMessage。
func (s *Store) SaveMessage(ctx context.Context, m contract.Message) error {
	if m.ID == "" || m.RoomID == "" {
		return errors.New("store: message 缺少 id 或 room_id")
	}
	meta, err := marshalMessageMeta(&m)
	if err != nil {
		return err
	}
	ts := m.CreatedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO messages
			(id, client_message_id, room_id, thread_id, work_id, cause_id,
			 from_id, from_kind, to_id, kind, text, meta_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, nullableText(m.ClientMessageID), m.RoomID, m.ThreadID,
		nullableText(m.WorkID), nullableText(m.CauseID), m.FromID,
		string(m.FromKind), nullableText(m.ToID), string(m.Kind),
		m.Text, meta, ts.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("store: 保存 message %s 失败: %w", m.ID, err)
	}
	return nil
}

// GetMessage 按 id 读取消息。查不到返回 orchestrator.ErrNotFound。
func (s *Store) GetMessage(ctx context.Context, messageID string) (contract.Message, error) {
	var (
		m                contract.Message
		clientMessageID  sql.NullString
		threadID         sql.NullString
		workID           sql.NullString
		causeID          sql.NullString
		toID             sql.NullString
		metaRaw          sql.NullString
		createdAtMs      int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, client_message_id, room_id, thread_id, work_id, cause_id,
		        from_id, from_kind, to_id, kind, text, meta_json, created_at
		 FROM messages WHERE id = ?`, messageID,
	).Scan(&m.ID, &clientMessageID, &m.RoomID, &threadID, &workID, &causeID,
		&m.FromID, &m.FromKind, &toID, &m.Kind, &m.Text, &metaRaw, &createdAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return contract.Message{}, orchestrator.ErrNotFound
	}
	if err != nil {
		return contract.Message{}, fmt.Errorf("store: 读取 message %s 失败: %w", messageID, err)
	}
	m.ClientMessageID = clientMessageID.String
	m.ThreadID = threadID.String
	m.WorkID = workID.String
	m.CauseID = causeID.String
	m.ToID = toID.String
	m.CreatedAt = time.UnixMilli(createdAtMs)
	if metaRaw.Valid && metaRaw.String != "" {
		mentions, replyTo, files, err := UnmarshalMessageMeta(metaRaw.String)
		if err != nil {
			return contract.Message{}, fmt.Errorf("store: 解析 message %s 的 meta 失败: %w", messageID, err)
		}
		m.Mentions = mentions
		m.ReplyTo = replyTo
		m.Files = files
	}
	return m, nil
}

// ---------------- Review ----------------

// SaveReview 保存一次审查结论。
//
// assignment_version 在同一事务里从 works 行读取：审查结论必须绑定"当时"的分工
// 版本，这样旧分工下的 approved 不会被误当作新分工的通过（§5.8 / §9.3）。
func (s *Store) SaveReview(ctx context.Context, r contract.Review) error {
	if r.ID == "" || r.WorkID == "" {
		return errors.New("store: review 缺少 id 或 work_id")
	}
	findingsJSON, err := json.Marshal(r.Findings)
	if err != nil {
		return fmt.Errorf("store: 序列化 review findings 失败: %w", err)
	}
	ts := r.CreatedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	skipped := 0
	if r.Skipped {
		skipped = 1
	}

	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var assignVer int64
		err := tx.QueryRowContext(ctx,
			`SELECT assignment_version FROM works WHERE id = ?`, r.WorkID).Scan(&assignVer)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: review %s 挂到不存在的 work %s: %w", r.ID, r.WorkID, orchestrator.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("store: 读取 work %s 的分工版本失败: %w", r.WorkID, err)
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO reviews
				(id, work_id, reviewer_id, assignment_version, artifact_version,
				 verdict, findings_json, skipped, ts)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO NOTHING`,
			r.ID, r.WorkID, r.Reviewer, assignVer, r.ArtifactVersion,
			string(r.Verdict), string(findingsJSON), skipped, ts.UnixMilli(),
		); err != nil {
			return fmt.Errorf("store: 保存 review %s 失败: %w", r.ID, err)
		}
		return nil
	})
}

// Reviews 读取一个 Work 的全部审查结论，按时间升序。
func (s *Store) Reviews(ctx context.Context, workID string) ([]contract.Review, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, reviewer_id, artifact_version, verdict, findings_json, skipped, ts
		 FROM reviews WHERE work_id = ? ORDER BY ts, id`, workID)
	if err != nil {
		return nil, fmt.Errorf("store: 列出 reviews 失败(work=%s): %w", workID, err)
	}
	defer rows.Close()

	var out []contract.Review
	for rows.Next() {
		var (
			r            contract.Review
			findingsJSON sql.NullString
			skipped      int
			tsMs         int64
		)
		if err := rows.Scan(&r.ID, &r.Reviewer, &r.ArtifactVersion, &r.Verdict,
			&findingsJSON, &skipped, &tsMs); err != nil {
			return nil, fmt.Errorf("store: 读取 review 行失败: %w", err)
		}
		r.WorkID = workID
		r.Skipped = skipped == 1
		r.CreatedAt = time.UnixMilli(tsMs)
		if findingsJSON.Valid && findingsJSON.String != "" {
			var findings string
			if err := json.Unmarshal([]byte(findingsJSON.String), &findings); err == nil {
				r.Findings = findings
			}
			// 解析失败按空 findings 处理：审查结论本体（verdict）不受影响。
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------- RoleAssignment ----------------

// SaveAssignment 记录一版分工，并同步 works 行的执行者/审查者/版本号。
//
// 同 (work_id, version) 重放写入按 upsert 处理：编排层重试不应造成第二行。
func (s *Store) SaveAssignment(ctx context.Context, workID string, a contract.RoleAssignment) error {
	if workID == "" {
		return errors.New("store: work_id 为空")
	}
	payload, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("store: 序列化 assignment 失败: %w", err)
	}
	at := a.At
	if at.IsZero() {
		at = time.Now()
	}

	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// 先确认 work 存在，并在同一事务里读出 work_json：列与 JSON 双写时
		// JSON 是权威（与 SaveWork 的单一来源原则一致），只改列会造出两套真相。
		// 变量名刻意区分：workPayload 是 works 行的 JSON，外层 payload 是本版分工的 JSON。
		var workPayload []byte
		if err := tx.QueryRowContext(ctx,
			`SELECT work_json FROM works WHERE id = ?`, workID).Scan(&workPayload); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("store: assignment 挂到不存在的 work %s: %w", workID, orchestrator.ErrNotFound)
			}
			return fmt.Errorf("store: 读取 work %s 失败: %w", workID, err)
		}
		w, err := decodeWork(workPayload)
		if err != nil {
			return fmt.Errorf("store: 解析 work %s 失败: %w", workID, err)
		}
		w.Assign = a
		if w.UpdatedAt.Before(at) {
			w.UpdatedAt = at
		}
		updated, err := json.Marshal(w)
		if err != nil {
			return fmt.Errorf("store: 序列化 work %s 失败: %w", workID, err)
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO assignments (work_id, version, assignment_json, created_at)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(work_id, version) DO UPDATE SET assignment_json = excluded.assignment_json`,
			workID, a.Version, string(payload), at.UnixMilli(),
		); err != nil {
			return fmt.Errorf("store: 保存 assignment(work=%s, v%d) 失败: %w", workID, a.Version, err)
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE works SET executor_id = ?, reviewer_id = ?, assignment_version = ?, work_json = ?
			 WHERE id = ?`,
			nullableText(a.Executor), nullableText(a.Reviewer), a.Version, string(updated), workID,
		); err != nil {
			return fmt.Errorf("store: 同步 work %s 的分工列失败: %w", workID, err)
		}
		return nil
	})
}

// Assignments 读取一个 Work 的全部分工历史，按版本升序（§5.8 / §12）。
func (s *Store) Assignments(ctx context.Context, workID string) ([]contract.RoleAssignment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT assignment_json FROM assignments WHERE work_id = ? ORDER BY version`, workID)
	if err != nil {
		return nil, fmt.Errorf("store: 列出 assignments 失败(work=%s): %w", workID, err)
	}
	defer rows.Close()

	var out []contract.RoleAssignment
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取 assignment 行失败: %w", err)
		}
		var a contract.RoleAssignment
		if err := json.Unmarshal(payload, &a); err != nil {
			return nil, fmt.Errorf("store: 解析 assignment_json 失败(work=%s): %w", workID, err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------- Pending（换人时撤销未处理项，§5.8） ----------------

// PendingFor 列出一个 Work 尚未处理完的投递与审批。
//
// 只查"还没发生"的：deliveries.status='pending' 与 approvals.status='pending'。
// 已经发进客户端的动作不在此列（外部副作用撤不回，见 orchestrator.Pending 注释）。
func (s *Store) PendingFor(ctx context.Context, workID string) ([]orchestrator.Pending, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.id, d.bot_id FROM deliveries d
		 JOIN messages m ON m.id = d.message_id
		 WHERE m.work_id = ? AND d.status = ?
		 ORDER BY d.created_at, d.id`, workID, DeliveryPending)
	if err != nil {
		return nil, fmt.Errorf("store: 查询待投递失败(work=%s): %w", workID, err)
	}
	defer rows.Close()

	out := make([]orchestrator.Pending, 0, 4)
	for rows.Next() {
		var p orchestrator.Pending
		if err := rows.Scan(&p.ID, &p.BotID); err != nil {
			return nil, fmt.Errorf("store: 读取待投递行失败: %w", err)
		}
		p.WorkID = workID
		p.Kind = orchestrator.PendingDelivery
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 审批行没有 bot_id 列，经 runs 关联取（审批总挂在某次唤醒的 Run 下）。
	approvalRows, err := s.db.QueryContext(ctx,
		`SELECT a.id, COALESCE(r.bot_id, '') FROM approvals a
		 LEFT JOIN runs r ON r.id = a.run_id
		 WHERE a.work_id = ? AND a.status = ?
		 ORDER BY a.created_at, a.id`, workID, "pending")
	if err != nil {
		return nil, fmt.Errorf("store: 查询待审批失败(work=%s): %w", workID, err)
	}
	defer approvalRows.Close()

	for approvalRows.Next() {
		var p orchestrator.Pending
		if err := approvalRows.Scan(&p.ID, &p.BotID); err != nil {
			return nil, fmt.Errorf("store: 读取待审批行失败: %w", err)
		}
		p.WorkID = workID
		p.Kind = orchestrator.PendingApproval
		out = append(out, p)
	}
	return out, approvalRows.Err()
}

// RevokePending 撤销一条尚未处理的投递或审批。
//
// pendingID 可能是投递 id 也可能是审批 id：先试投递，不命中再试审批，
// 两个都不命中返回 orchestrator.ErrNotFound。
func (s *Store) RevokePending(ctx context.Context, pendingID, reason string) error {
	now := time.Now()
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// 先试投递：只撤 pending，已领取/已完成的由调用方按各自状态处理。
		res, err := tx.ExecContext(ctx,
			`UPDATE deliveries SET status = ?, last_error = ?, updated_at = ?
			 WHERE id = ? AND status = ?`,
			DeliveryCancelled, reason, now.UnixMilli(), pendingID, DeliveryPending)
		if err != nil {
			return fmt.Errorf("store: 撤销投递 %s 失败: %w", pendingID, err)
		}
		if n, err := rowsAffected(res); err != nil {
			return err
		} else if n == 1 {
			return nil
		}

		// 再试审批。
		res, err = tx.ExecContext(ctx,
			`UPDATE approvals SET status = ? WHERE id = ? AND status = ?`,
			"cancelled", pendingID, "pending")
		if err != nil {
			return fmt.Errorf("store: 撤销审批 %s 失败: %w", pendingID, err)
		}
		if n, err := rowsAffected(res); err != nil {
			return err
		} else if n == 1 {
			return nil
		}
		return orchestrator.ErrNotFound
	})
	return err
}

// ---------------- MarkInputSeen（护栏一，§5.3） ----------------

// MarkInputSeen 幂等登记 (message_id, bot_id)。已存在返回 seen=true。
//
// 原子性由 input_seen 的 PRIMARY KEY (message_id, bot_id) + INSERT OR IGNORE 保证：
// 串行写队列下同一时刻只有一个写入者，唯一约束兜底重启后的重复登记。
// 广播类消息（没有投递行的）也能登记——这是它与 deliveries 的本质区别。
func (s *Store) MarkInputSeen(ctx context.Context, messageID, botID string) (bool, error) {
	if messageID == "" || botID == "" {
		return false, errors.New("store: MarkInputSeen 需要 message_id 与 bot_id")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO input_seen (message_id, bot_id, seen_at) VALUES (?, ?, ?)
		 ON CONFLICT(message_id, bot_id) DO NOTHING`,
		messageID, botID, time.Now().UnixMilli(),
	)
	if err != nil {
		return false, fmt.Errorf("store: 登记 input seen(%s, %s) 失败: %w", messageID, botID, err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// 编译期断言：*Store 必须满足编排层接口，方法签名漂移在编译期暴露而不是组装期。
var _ orchestrator.Store = (*Store)(nil)
