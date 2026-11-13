package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"mesh/internal/contract"
)

// 用量与成本（§9.6）。
//
// 核心约束：未知 ≠ 0。
// 四个桌面客户端首版不提供逐 Run 的 token 与金额，这类通道的用量必须如实记为
// "未知"（tokens_* / amount 写 NULL），绝不能写 0——写 0 会让统计与预算看起来
// "有数据且没花钱"，进而让用户误以为金额硬顶生效了。§9.6 明确禁止这种假象。
//
// 因此：
//   - contract.Usage.Known=false 时，tokens_in/tokens_out/cached_tokens/amount/currency/
//     price_version 全部写 NULL，status 记 UsageUnknown；
//   - 只有 Known=true 才写具体数字，此时 0 是合法值（真的用了 0 token）。
//
// 预算流程：调用前 ReserveUsage 预留额度，调用后 SettleUsage 结算实际值（§9.6）。
// 预留与结算分开，是为了在调用失败/进程崩溃时仍能看出"这笔钱可能已经花了"。

// usage.status 取值。
const (
	// UsageReserved 已预留额度，调用尚未结算。
	UsageReserved = "reserved"
	// UsageSettled 已结算，金额与 token 可信。
	UsageSettled = "settled"
	// UsageUnknown 该通道无法计量，金额与 token 未知（NULL，不是 0）。
	UsageUnknown = "unknown"
)

// ErrRequestIDRequired 表示缺少幂等键。
var ErrRequestIDRequired = errors.New("store: usage 记录缺少 request_id")

// UsageRecord 是一条待写入的用量记录。
// 上层为每次模型/接口调用生成一个 request_id，重复提交不得重复计费（§9.1 唯一约束）。
type UsageRecord struct {
	RequestID      string
	WorkID         string
	RunID          string
	Provider       string
	Model          string
	ReservedAmount int64         // 预留额度，单位同 amount（微单位）
	Usage          *contract.Usage // 结算用量；nil 或 Known=false 表示未知
}

// RecordUsage 一次性写入一条已结算（或已确认未知）的用量记录。
//
// 返回 true 表示新写入，false 表示 request_id 已存在、本次被幂等丢弃。
// 不覆盖已有记录：重复调用不应改变已结算的金额。
func (s *Store) RecordUsage(ctx context.Context, rec UsageRecord, now time.Time) (bool, error) {
	if rec.RequestID == "" {
		return false, ErrRequestIDRequired
	}
	u := rec.Usage
	known := u != nil && u.Known

	status := UsageUnknown
	if known {
		status = UsageSettled
	}

	var inserted bool
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO usage
				(id, work_id, run_id, request_id, provider, model, reserved_amount,
				 tokens_in, tokens_out, cached_tokens, amount, currency, price_version,
				 status, created_at, settled_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (request_id) DO NOTHING`,
			rec.RequestID, // 直接用 request_id 作主键：一条调用一条记录，天然幂等
			nullableText(rec.WorkID), nullableText(rec.RunID), rec.RequestID,
			providerOf(rec, u), modelOf(rec, u), rec.ReservedAmount,
			knownInt64(known, u, func(v *contract.Usage) int64 { return v.TokensIn }),
			knownInt64(known, u, func(v *contract.Usage) int64 { return v.TokensOut }),
			knownInt64(known, u, func(v *contract.Usage) int64 { return v.CacheReadTokens }),
			knownAmount(known, u),
			knownText(known, u, func(v *contract.Usage) string { return v.Currency }),
			knownText(known, u, func(v *contract.Usage) string { return v.PriceVersion }),
			status, now.UnixMilli(), now.UnixMilli(),
		)
		if err != nil {
			return fmt.Errorf("store: 写入用量失败(request=%s): %w", rec.RequestID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: 读取 RowsAffected 失败: %w", err)
		}
		inserted = n == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return inserted, nil
}

// ReserveUsage 在调用前预留额度，写入 status=reserved 的记录。
//
// 预留金额可能大于实际金额（预估值）；SettleUsage 之后以实际值覆盖 amount，
// 但 reserved_amount 保留，便于事后核对估算偏差。
func (s *Store) ReserveUsage(ctx context.Context, rec UsageRecord, now time.Time) error {
	if rec.RequestID == "" {
		return ErrRequestIDRequired
	}
	u := rec.Usage
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO usage
				(id, work_id, run_id, request_id, provider, model, reserved_amount,
				 tokens_in, tokens_out, cached_tokens, amount, currency, price_version,
				 status, created_at, settled_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL, NULL, ?, NULL, ?, ?, NULL)
			 ON CONFLICT (request_id) DO NOTHING`,
			rec.RequestID, nullableText(rec.WorkID), nullableText(rec.RunID), rec.RequestID,
			providerOf(rec, u), modelOf(rec, u), rec.ReservedAmount,
			knownText(u != nil && u.Known, u, func(v *contract.Usage) string { return v.Currency }),
			UsageReserved, now.UnixMilli(),
		)
		if err != nil {
			return fmt.Errorf("store: 预留用量失败(request=%s): %w", rec.RequestID, err)
		}
		return nil
	})
}

// SettleUsage 在调用后结算实际用量，覆盖预留记录。
//
// u == nil 或 u.Known == false 时按"未知"结算：tokens 与 amount 写 NULL，status=unknown。
// 返回 false 表示没有对应的预留记录（例如进程在预留前就崩溃了），
// 此时上层可选择直接 RecordUsage 补一条。
func (s *Store) SettleUsage(ctx context.Context, requestID string, u *contract.Usage, now time.Time) (bool, error) {
	if requestID == "" {
		return false, ErrRequestIDRequired
	}
	known := u != nil && u.Known
	status := UsageUnknown
	if known {
		status = UsageSettled
	}

	var updated bool
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE usage
			    SET tokens_in = ?, tokens_out = ?, cached_tokens = ?, amount = ?,
			        currency = COALESCE(?, currency), price_version = COALESCE(?, price_version),
			        status = ?, settled_at = ?
			  WHERE request_id = ?`,
			knownInt64(known, u, func(v *contract.Usage) int64 { return v.TokensIn }),
			knownInt64(known, u, func(v *contract.Usage) int64 { return v.TokensOut }),
			knownInt64(known, u, func(v *contract.Usage) int64 { return v.CacheReadTokens }),
			knownAmount(known, u),
			knownText(known, u, func(v *contract.Usage) string { return v.Currency }),
			knownText(known, u, func(v *contract.Usage) string { return v.PriceVersion }),
			status, now.UnixMilli(), requestID,
		)
		if err != nil {
			return fmt.Errorf("store: 结算用量失败(request=%s): %w", requestID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: 读取 RowsAffected 失败: %w", err)
		}
		updated = n == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return updated, nil
}

// CurrencyTotal 单一币种的金额合计。
// 不同币种绝不相加——那会得出一个没有意义的数字。
type CurrencyTotal struct {
	Currency     string
	AmountMicros int64
	Calls        int
}

// UsageSummary 是一段时间/一个 Work 的用量汇总。
//
// 关键字段是 UnknownAmountCalls：金额未知的调用条数。UI 必须把它显示出来
// （例如"另有 12 次调用金额未知"），不能只显示已知部分的和然后当作总额（§9.6）。
type UsageSummary struct {
	Calls              int             // 记录条数
	KnownAmountCalls   int             // 金额已知的调用数
	UnknownAmountCalls int             // 金额未知的调用数
	ReservedCalls      int             // 仍未结算的预留数
	ByCurrency         []CurrencyTotal // 按币种分组的金额合计
	TokensIn           int64           // 已知行的 token 之和
	TokensOut          int64
	CacheReadTokens    int64
	CallsWithTokens    int // 有 token 数据的调用数，用于说明 token 覆盖率
}

// UsageByWork 汇总某个 Work 的用量。
func (s *Store) UsageByWork(ctx context.Context, workID string) (UsageSummary, error) {
	if workID == "" {
		return UsageSummary{}, errors.New("store: UsageByWork 需要 work_id")
	}
	return s.queryUsage(ctx,
		`SELECT status, currency, amount, tokens_in, tokens_out, cached_tokens
		   FROM usage WHERE work_id = ?`, workID)
}

// UsageByRun 汇总某个 Run 的用量。
func (s *Store) UsageByRun(ctx context.Context, runID string) (UsageSummary, error) {
	if runID == "" {
		return UsageSummary{}, errors.New("store: UsageByRun 需要 run_id")
	}
	return s.queryUsage(ctx,
		`SELECT status, currency, amount, tokens_in, tokens_out, cached_tokens
		   FROM usage WHERE run_id = ?`, runID)
}

// UsageSince 汇总某个时间点之后的全部用量（成本面板用）。
func (s *Store) UsageSince(ctx context.Context, since time.Time) (UsageSummary, error) {
	return s.queryUsage(ctx,
		`SELECT status, currency, amount, tokens_in, tokens_out, cached_tokens
		   FROM usage WHERE created_at >= ?`, since.UnixMilli())
}

// CostBucket 成本聚合里的一个分组。
//
// Key 的含义由调用方决定：UsageByBot 是 bot_id，UsageByRoom 是 room_id，
// UsageByDay 是 UTC 日期（yyyy-mm-dd）。空 Key 表示"关联不到该维度"的记录，
// 单独成桶，绝不并入任何真实 bot / 房间。
type CostBucket struct {
	Key     string
	Summary UsageSummary
}

// UsageByBot 按 bot 聚合用量（§9.1 成本面板：哪个 bot 花了多少）。
//
// usage 表没有 bot_id，必须经 runs 关联。LEFT JOIN 是有意的：run_id 为空
// 或 Run 已不存在的用量仍要计入总账，只是归到空 Key 桶里。
func (s *Store) UsageByBot(ctx context.Context, since time.Time) ([]CostBucket, error) {
	return s.queryUsageGrouped(ctx,
		`SELECT COALESCE(r.bot_id, '') AS k,
		        u.status, u.currency, u.amount, u.tokens_in, u.tokens_out, u.cached_tokens
		   FROM usage u
		   LEFT JOIN runs r ON r.id = u.run_id
		  WHERE u.created_at >= ?
		  ORDER BY k`, since.UnixMilli())
}

// UsageByRoom 按房间聚合用量。同样经 runs 关联。
func (s *Store) UsageByRoom(ctx context.Context, since time.Time) ([]CostBucket, error) {
	return s.queryUsageGrouped(ctx,
		`SELECT COALESCE(r.room_id, '') AS k,
		        u.status, u.currency, u.amount, u.tokens_in, u.tokens_out, u.cached_tokens
		   FROM usage u
		   LEFT JOIN runs r ON r.id = u.run_id
		  WHERE u.created_at >= ?
		  ORDER BY k`, since.UnixMilli())
}

// UsageByDay 按 UTC 自然日聚合用量。
//
// 按 UTC 切日而不是本地时区：库里时间统一是 UTC 毫秒，用本地时区切会让
// "一条记录算哪一天"随部署机器而变，换台机器对账就对不上。README 已记录该口径。
func (s *Store) UsageByDay(ctx context.Context, since time.Time) ([]CostBucket, error) {
	return s.queryUsageGrouped(ctx,
		`SELECT strftime('%Y-%m-%d', created_at / 1000, 'unixepoch') AS k,
		        status, currency, amount, tokens_in, tokens_out, cached_tokens
		   FROM usage
		  WHERE created_at >= ?
		  ORDER BY k`, since.UnixMilli())
}

// usageAcc 是逐行累加器：把"已知 / 未知 / 币种 / token"的口径收在一处，
// 保证单桶汇总与分桶汇总不会算出两套数字。
type usageAcc struct {
	sum      UsageSummary
	byCur    map[string]*CurrencyTotal
	curOrder []string
}

func newUsageAcc() *usageAcc {
	return &usageAcc{byCur: make(map[string]*CurrencyTotal)}
}

func (a *usageAcc) add(status string, currency sql.NullString, amount, tokensIn, tokensOut, cached sql.NullInt64) {
	a.sum.Calls++
	if status == UsageReserved {
		a.sum.ReservedCalls++
	}
	if amount.Valid {
		a.sum.KnownAmountCalls++
		// 金额有值但没记币种：归到空币种桶，不并进任何真实币种（不同币种绝不相加）。
		c := currency.String
		t, ok := a.byCur[c]
		if !ok {
			t = &CurrencyTotal{Currency: c}
			a.byCur[c] = t
			a.curOrder = append(a.curOrder, c)
		}
		t.AmountMicros += amount.Int64
		t.Calls++
	} else {
		a.sum.UnknownAmountCalls++
	}
	if tokensIn.Valid || tokensOut.Valid || cached.Valid {
		a.sum.CallsWithTokens++
		a.sum.TokensIn += tokensIn.Int64
		a.sum.TokensOut += tokensOut.Int64
		a.sum.CacheReadTokens += cached.Int64
	}
}

func (a *usageAcc) result() UsageSummary {
	out := a.sum
	for _, c := range a.curOrder {
		out.ByCurrency = append(out.ByCurrency, *a.byCur[c])
	}
	return out
}

// queryUsage 汇总单一口径（某个 Work / Run / 时间段）的用量。
//
// 为什么在 Go 里聚合而不在 SQL 里 GROUP BY：需要在同一个结果里区分
// "金额已知"与"金额未知"两种行，并且要按币种分组后再算未知条数；
// 数据量是单机自用规模（万级），一次全表扫描完全够用，SQL 里写三段
// UNION 反而更容易出错。
func (s *Store) queryUsage(ctx context.Context, query string, args ...any) (UsageSummary, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return UsageSummary{}, fmt.Errorf("store: 查询用量失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	acc := newUsageAcc()
	for rows.Next() {
		var (
			status    string
			currency  sql.NullString
			amount    sql.NullInt64
			tokensIn  sql.NullInt64
			tokensOut sql.NullInt64
			cached    sql.NullInt64
		)
		if err := rows.Scan(&status, &currency, &amount, &tokensIn, &tokensOut, &cached); err != nil {
			return UsageSummary{}, fmt.Errorf("store: 读取用量行失败: %w", err)
		}
		acc.add(status, currency, amount, tokensIn, tokensOut, cached)
	}
	if err := rows.Err(); err != nil {
		return UsageSummary{}, fmt.Errorf("store: 遍历用量失败: %w", err)
	}
	return acc.result(), nil
}

// queryUsageGrouped 把结果第一列当作分组键汇总用量，按出现顺序返回。
func (s *Store) queryUsageGrouped(ctx context.Context, query string, args ...any) ([]CostBucket, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 分组查询用量失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	accs := make(map[string]*usageAcc)
	var order []string
	for rows.Next() {
		var (
			key       string
			status    string
			currency  sql.NullString
			amount    sql.NullInt64
			tokensIn  sql.NullInt64
			tokensOut sql.NullInt64
			cached    sql.NullInt64
		)
		if err := rows.Scan(&key, &status, &currency, &amount, &tokensIn, &tokensOut, &cached); err != nil {
			return nil, fmt.Errorf("store: 读取分组用量行失败: %w", err)
		}
		acc, ok := accs[key]
		if !ok {
			acc = newUsageAcc()
			accs[key] = acc
			order = append(order, key)
		}
		acc.add(status, currency, amount, tokensIn, tokensOut, cached)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历分组用量失败: %w", err)
	}

	out := make([]CostBucket, 0, len(order))
	for _, k := range order {
		out = append(out, CostBucket{Key: k, Summary: accs[k].result()})
	}
	return out, nil
}

// ---------- 内部工具 ----------

func providerOf(rec UsageRecord, u *contract.Usage) string {
	if u != nil && u.Provider != "" {
		return u.Provider
	}
	return rec.Provider
}

func modelOf(rec UsageRecord, u *contract.Usage) string {
	if u != nil && u.Model != "" {
		return u.Model
	}
	return rec.Model
}

// knownInt64 在已知时返回真实值（含合法的 0），未知时返回 nil（SQL NULL）。
func knownInt64(known bool, u *contract.Usage, pick func(*contract.Usage) int64) any {
	if !known || u == nil {
		return nil
	}
	return pick(u)
}

// knownAmount 返回金额（微单位）。未知时 nil——绝不返回 0。
func knownAmount(known bool, u *contract.Usage) any {
	if !known || u == nil {
		return nil
	}
	return u.AmountMicros
}

func knownText(known bool, u *contract.Usage, pick func(*contract.Usage) string) any {
	if !known || u == nil {
		return nil
	}
	s := pick(u)
	if s == "" {
		return nil
	}
	return s
}
