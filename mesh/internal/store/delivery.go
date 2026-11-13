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

// 投递状态机（§5.6 / §9.1）。
//
//	pending  --claim-->  running  --成功-->  done
//	                        |
//	                        +--可重试-->  pending（attempt < MaxDeliveryAttempt）
//	                        +--不可重试/超限--> failed
//	pending / running --角色变更或取消--> cancelled
//
// 一条投递记录 = "这条消息该被这个 bot 处理"。唯一约束 (message_id, bot_id)
// 保证同一条消息对同一个 bot 不会产生两条投递。
const (
	DeliveryPending   = "pending"
	DeliveryRunning   = "running"
	DeliveryDone      = "done"
	DeliveryFailed    = "failed"
	DeliveryCancelled = "cancelled"
)

// Run 状态。interrupted 只由重启恢复写入：进程被杀时正在跑的 Run 无法得知
// 真实结局，只能如实标成"中断"，不得当作成功或失败（§5.6）。
const (
	RunRunning     = "running"
	RunDone        = "done"
	RunFailed      = "failed"
	RunInterrupted = "interrupted"
)

// MaxDeliveryAttempt 投递重试上限（§5.6：失败重试最多 3 次）。
const MaxDeliveryAttempt = 3

// ErrDuplicateClientMessage 表示 client_message_id 已存在，本次是重投。
// 上层应当把它当作"已处理"，不要报错给用户，也不要再建 Work（§5.6）。
var ErrDuplicateClientMessage = errors.New("store: 该 client_message_id 的消息已存在")

// retryBackoff 是第 n 次失败后的等待时长（attempt 从 1 开始）。
var retryBackoff = []time.Duration{
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
}

// Delivery 一条投递记录。
type Delivery struct {
	ID            string
	MessageID     string
	BotID         string
	WorkID        string
	RoomID        string
	Status        string
	Attempt       int
	RunID         string
	NextAttemptAt time.Time // 零值 = 没有安排重试时间
}

// CreateMessage 在同一个事务里写入消息与待投递记录（§5.6）。
//
// 为什么必须同一事务：消息落库但投递没落库，消息就永远不会被处理；
// 投递落库但消息没落库，worker 领到投递却读不到内容。
// 事务提交之后调用方才可以唤醒 worker。
//
// targets 是这条消息要投递给哪些 bot。为空表示只入库不投递（例如用户自己发的
// 通知类消息）。重复的 bot 会被去重，避免撞 (message_id, bot_id) 唯一约束。
func (s *Store) CreateMessage(ctx context.Context, msg *contract.Message, targets []string) error {
	if msg == nil {
		return errors.New("store: message 为空")
	}
	if msg.ID == "" || msg.RoomID == "" {
		return errors.New("store: message 缺少 id 或 room_id")
	}

	meta, err := marshalMessageMeta(msg)
	if err != nil {
		return err
	}
	now := time.Now()

	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if msg.ClientMessageID != "" {
			var exists int
			err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM messages WHERE client_message_id = ?`,
				msg.ClientMessageID).Scan(&exists)
			if err != nil {
				return fmt.Errorf("store: 检查 client_message_id 失败: %w", err)
			}
			if exists > 0 {
				return ErrDuplicateClientMessage
			}
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages
				(id, client_message_id, room_id, thread_id, work_id, cause_id,
				 from_id, from_kind, to_id, kind, text, meta_json, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			msg.ID, nullableText(msg.ClientMessageID), msg.RoomID, msg.ThreadID,
			nullableText(msg.WorkID), nullableText(msg.CauseID), msg.FromID,
			string(msg.FromKind), nullableText(msg.ToID), string(msg.Kind),
			msg.Text, meta, now.UnixMilli(),
		); err != nil {
			return fmt.Errorf("store: 写入消息失败: %w", err)
		}

		seen := make(map[string]struct{}, len(targets))
		for _, botID := range targets {
			if botID == "" {
				continue
			}
			if _, dup := seen[botID]; dup {
				continue
			}
			seen[botID] = struct{}{}

			// 投递 id 由上层业务 id 派生，保证重放同一事务不会造出不同 id。
			deliveryID := msg.ID + ":" + botID
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO deliveries
					(id, message_id, bot_id, status, attempt, run_id,
					 next_attempt_at, last_error, created_at, updated_at)
				 VALUES (?, ?, ?, ?, 0, NULL, NULL, NULL, ?, ?)`,
				deliveryID, msg.ID, botID, DeliveryPending, now.UnixMilli(), now.UnixMilli(),
			); err != nil {
				return fmt.Errorf("store: 写入投递失败(bot=%s): %w", botID, err)
			}
		}
		return nil
	})
}

// ClaimDelivery 原子领取一条待投递记录，并在同一事务内创建对应的 Run。
//
// 原子性怎么保证：SELECT 与 UPDATE ... WHERE status='pending' 在同一个写事务内，
// 且存储层只有一个串行写入队列，两个 worker 不可能领到同一条记录。
//
// 只领有 work_id 的消息：Run 必须挂在 Work 下（§2），没有 Work 的消息应先由上层
// 在同一事务里建好 Work 再产生投递，这是 §5.6 规定的顺序。
//
// 队列为空返回 (nil, nil)。newRunID 由上层生成（存储层不造业务 id）。
func (s *Store) ClaimDelivery(ctx context.Context, now time.Time, newRunID string) (*Delivery, error) {
	if newRunID == "" {
		return nil, errors.New("store: ClaimDelivery 需要上层传入 run id")
	}
	nowMs := now.UnixMilli()

	var claimed *Delivery
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var (
			d         Delivery
			workID    sql.NullString
			roomID    sql.NullString
			messageID string
		)
		err := tx.QueryRowContext(ctx,
			`SELECT d.id, d.message_id, d.bot_id, d.attempt, m.work_id, m.room_id
			   FROM deliveries d
			   JOIN messages m ON m.id = d.message_id
			  WHERE d.status = ?
			    AND m.work_id IS NOT NULL
			    AND (d.next_attempt_at IS NULL OR d.next_attempt_at <= ?)
			  ORDER BY d.created_at, d.id
			  LIMIT 1`,
			DeliveryPending, nowMs,
		).Scan(&d.ID, &messageID, &d.BotID, &d.Attempt, &workID, &roomID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // 队列为空，不算错误
		}
		if err != nil {
			return fmt.Errorf("store: 查询待投递失败: %w", err)
		}
		d.MessageID = messageID
		d.WorkID = workID.String
		d.RoomID = roomID.String
		d.Status = DeliveryRunning
		d.Attempt++
		d.RunID = newRunID

		// 分工版本与配置版本必须记进 Run：审批与审查都按这两个版本校验（§5.8 / §9.3）。
		var assignmentVersion int
		if err := tx.QueryRowContext(ctx,
			`SELECT assignment_version FROM works WHERE id = ?`, d.WorkID,
		).Scan(&assignmentVersion); err != nil {
			return fmt.Errorf("store: 读取 works.assignment_version 失败(work=%s): %w", d.WorkID, err)
		}
		var configVersion int
		err = tx.QueryRowContext(ctx,
			`SELECT config_version FROM bots WHERE id = ?`, d.BotID,
		).Scan(&configVersion)
		if errors.Is(err, sql.ErrNoRows) {
			// bot 尚未登记（首次使用）：按版本 1 记录，不阻塞投递。
			configVersion = 1
		} else if err != nil {
			return fmt.Errorf("store: 读取 bots.config_version 失败(bot=%s): %w", d.BotID, err)
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO runs
				(id, work_id, bot_id, room_id, message_id, assignment_version,
				 config_version, session_ref, status, started_at, ended_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, NULL)`,
			newRunID, d.WorkID, d.BotID, d.RoomID, messageID,
			assignmentVersion, configVersion, RunRunning, nowMs,
		); err != nil {
			return fmt.Errorf("store: 创建 Run 失败: %w", err)
		}

		// status='pending' 是乐观并发保护：即使将来出现第二个写者，重复领取也会失败。
		res, err := tx.ExecContext(ctx,
			`UPDATE deliveries
			    SET status = ?, attempt = ?, run_id = ?, updated_at = ?
			  WHERE id = ? AND status = ?`,
			DeliveryRunning, d.Attempt, newRunID, nowMs, d.ID, DeliveryPending,
		)
		if err != nil {
			return fmt.Errorf("store: 领取投递失败: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: 读取 RowsAffected 失败: %w", err)
		}
		if n != 1 {
			return fmt.Errorf("store: 投递 %s 已被其他 worker 领取", d.ID)
		}

		claimed = &d
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// FinishRun 结束一个 Run，并同步收尾对应的投递记录。
// status 取 RunDone 或 RunFailed。Run 的结局与投递的结局分开写：
// Run 失败不一定意味着投递要重试（例如业务上已判定不可重试），由调用方决定是否再调 MarkDeliveryFailed。
func (s *Store) FinishRun(ctx context.Context, runID, status string, now time.Time) error {
	if status != RunDone && status != RunFailed {
		return fmt.Errorf("store: FinishRun 不接受状态 %q（只允许 %s / %s）", status, RunDone, RunFailed)
	}
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE runs SET status = ?, ended_at = ? WHERE id = ?`,
			status, now.UnixMilli(), runID,
		); err != nil {
			return fmt.Errorf("store: 结束 Run 失败: %w", err)
		}
		return nil
	})
}

// SetRunSession 记录 Run 绑定到的客户端会话，供重连后重新关联（§4.2.2）。
func (s *Store) SetRunSession(ctx context.Context, runID, sessionRef string) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE runs SET session_ref = ? WHERE id = ?`, sessionRef, runID,
		); err != nil {
			return fmt.Errorf("store: 写入 session_ref 失败: %w", err)
		}
		return nil
	})
}

// MarkDeliveryDone 把投递标记为完成。
func (s *Store) MarkDeliveryDone(ctx context.Context, deliveryID string, now time.Time) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE deliveries SET status = ?, last_error = NULL, next_attempt_at = NULL, updated_at = ?
			  WHERE id = ?`,
			DeliveryDone, now.UnixMilli(), deliveryID,
		)
		if err != nil {
			return fmt.Errorf("store: 标记投递完成失败: %w", err)
		}
		if err := expectOneRow(res, "投递", deliveryID); err != nil {
			return err
		}
		return nil
	})
}

// MarkDeliveryFailed 记录一次投递失败，返回处理后的状态（pending 或 failed）。
//
// 重试上限 MaxDeliveryAttempt：第 3 次失败后不再重试，标成 failed 并把原因留在
// last_error 里，UI 必须显示出来，不能静默丢弃（§5.6）。
func (s *Store) MarkDeliveryFailed(ctx context.Context, deliveryID, cause string, now time.Time) (string, error) {
	var status string
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var attempt int
		err := tx.QueryRowContext(ctx,
			`SELECT attempt FROM deliveries WHERE id = ?`, deliveryID).Scan(&attempt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: 投递不存在: %s", deliveryID)
		}
		if err != nil {
			return fmt.Errorf("store: 读取投递失败: %w", err)
		}

		if attempt >= MaxDeliveryAttempt {
			status = DeliveryFailed
			if _, err := tx.ExecContext(ctx,
				`UPDATE deliveries SET status = ?, last_error = ?, next_attempt_at = NULL, updated_at = ?
				  WHERE id = ?`,
				status, cause, now.UnixMilli(), deliveryID,
			); err != nil {
				return fmt.Errorf("store: 标记投递失败失败: %w", err)
			}
			return nil
		}

		status = DeliveryPending
		delay := backoffFor(attempt)
		if _, err := tx.ExecContext(ctx,
			`UPDATE deliveries SET status = ?, last_error = ?, next_attempt_at = ?, updated_at = ?
			  WHERE id = ?`,
			status, cause, now.Add(delay).UnixMilli(), now.UnixMilli(), deliveryID,
		); err != nil {
			return fmt.Errorf("store: 安排投递重试失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return status, nil
}

// DeliveryLastError 读取投递最近一次失败原因（last_error）。
// 投递不存在、从未失败或原因为空时返回空串（调用方按"未知"处理，不猜）。
func (s *Store) DeliveryLastError(ctx context.Context, deliveryID string) (string, error) {
	var lastErr sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT last_error FROM deliveries WHERE id = ?`, deliveryID,
	).Scan(&lastErr)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: 读取投递失败原因失败(%s): %w", deliveryID, err)
	}
	return lastErr.String, nil
}

// CancelDelivery 取消一条投递（用户撤回、角色变更等）。
// 已完成的投递不改成 cancelled，避免篡改历史。
func (s *Store) CancelDelivery(ctx context.Context, deliveryID, reason string, now time.Time) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE deliveries
			    SET status = ?, last_error = ?, next_attempt_at = NULL, updated_at = ?
			  WHERE id = ? AND status IN (?, ?)`,
			DeliveryCancelled, reason, now.UnixMilli(), deliveryID,
			DeliveryPending, DeliveryRunning,
		); err != nil {
			return fmt.Errorf("store: 取消投递失败: %w", err)
		}
		return nil
	})
}

// CancelWorkDeliveries 撤销某个 Work 下尚未处理的投递（§5.8 换人时必须撤销旧角色
// 未处理的投递与审批）。botID 为空表示撤销该 Work 下所有未处理投递。
// 返回被撤销的条数。
func (s *Store) CancelWorkDeliveries(ctx context.Context, workID, botID, reason string, now time.Time) (int, error) {
	var n int64
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE deliveries
			    SET status = ?, last_error = ?, next_attempt_at = NULL, updated_at = ?
			  WHERE status IN (?, ?)
			    AND message_id IN (SELECT id FROM messages WHERE work_id = ?)
			    AND (? = '' OR bot_id = ?)`,
			DeliveryCancelled, reason, now.UnixMilli(),
			DeliveryPending, DeliveryRunning, workID, botID, botID,
		)
		if err != nil {
			return fmt.Errorf("store: 撤销 Work 投递失败: %w", err)
		}
		n, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: 读取 RowsAffected 失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// RecoverReport 是一次重启恢复的结果，用于启动日志与 UI 提示。
type RecoverReport struct {
	InterruptedRuns    int
	RequeuedDeliveries int
	FailedDeliveries   int
}

// RecoverInterrupted 在进程启动时修复上次非正常退出留下的状态（§5.6）。
//
// 两件事：
//  1. 所有还是 running 的 Run 标成 interrupted——进程被杀时它们的真实结局不可知，
//     只能如实记"中断"，绝不能当成成功。
//  2. 所有还是 running 的投递退回 pending 以便重新领取；但重试次数已达上限的直接
//     判 failed，避免崩溃循环无限重试。
//
// 必须在开始领取投递之前调用。
func (s *Store) RecoverInterrupted(ctx context.Context, now time.Time) (RecoverReport, error) {
	var rep RecoverReport
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		nowMs := now.UnixMilli()

		res, err := tx.ExecContext(ctx,
			`UPDATE runs SET status = ?, ended_at = ? WHERE status = ?`,
			RunInterrupted, nowMs, RunRunning,
		)
		if err != nil {
			return fmt.Errorf("store: 标记中断 Run 失败: %w", err)
		}
		if rep.InterruptedRuns, err = rowsAffected(res); err != nil {
			return err
		}

		res, err = tx.ExecContext(ctx,
			`UPDATE deliveries
			    SET status = ?, last_error = ?, next_attempt_at = NULL, updated_at = ?
			  WHERE status = ? AND attempt >= ?`,
			DeliveryFailed,
			"进程重启时该投递仍未完成，且重试次数已达上限",
			nowMs, DeliveryRunning, MaxDeliveryAttempt,
		)
		if err != nil {
			return fmt.Errorf("store: 标记超限投递失败: %w", err)
		}
		if rep.FailedDeliveries, err = rowsAffected(res); err != nil {
			return err
		}

		res, err = tx.ExecContext(ctx,
			`UPDATE deliveries
			    SET status = ?, last_error = ?, next_attempt_at = NULL, updated_at = ?
			  WHERE status = ?`,
			DeliveryPending,
			"进程重启时该投递仍在运行，已重新排队",
			nowMs, DeliveryRunning,
		)
		if err != nil {
			return fmt.Errorf("store: 重新排队投递失败: %w", err)
		}
		if rep.RequeuedDeliveries, err = rowsAffected(res); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return RecoverReport{}, err
	}
	return rep, nil
}

// ---------- 内部工具 ----------

// messageMeta 是 messages.meta_json 的内容。
// 方案的表草图没有给 mentions / reply_to / files 建列，但它们属于消息语义且必须持久化，
// 因此放进 meta_json；字段名与 contract.Message 的 JSON 名保持一致，便于还原。
type messageMeta struct {
	Mentions []string `json:"mentions,omitempty"`
	ReplyTo  string   `json:"reply_to,omitempty"`
	Files    []string `json:"files,omitempty"`
}

func marshalMessageMeta(msg *contract.Message) (string, error) {
	if len(msg.Mentions) == 0 && msg.ReplyTo == "" && len(msg.Files) == 0 {
		return "", nil
	}
	b, err := json.Marshal(messageMeta{Mentions: msg.Mentions, ReplyTo: msg.ReplyTo, Files: msg.Files})
	if err != nil {
		return "", fmt.Errorf("store: 序列化消息 meta 失败: %w", err)
	}
	return string(b), nil
}

// UnmarshalMessageMeta 供上层读回 messages.meta_json。
func UnmarshalMessageMeta(raw string) (mentions []string, replyTo string, files []string, err error) {
	if raw == "" {
		return nil, "", nil, nil
	}
	var m messageMeta
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, "", nil, fmt.Errorf("store: 解析消息 meta 失败: %w", err)
	}
	return m.Mentions, m.ReplyTo, m.Files, nil
}

func backoffFor(attempt int) time.Duration {
	if attempt <= 0 {
		return retryBackoff[0]
	}
	if attempt > len(retryBackoff) {
		return retryBackoff[len(retryBackoff)-1]
	}
	return retryBackoff[attempt-1]
}

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func expectOneRow(res sql.Result, what, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取 RowsAffected 失败: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("store: %s不存在或已被修改: %s", what, id)
	}
	return nil
}

func rowsAffected(res sql.Result) (int, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: 读取 RowsAffected 失败: %w", err)
	}
	return int(n), nil
}
