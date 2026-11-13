package orchestrator

import (
	"context"
	"errors"

	"mesh/internal/contract"
)

// ErrNotFound 记录不存在。
// 实现方必须原样返回本错误（不要自造字符串），编排层用 errors.Is 判断。
var ErrNotFound = errors.New("orchestrator: 记录不存在")

// PendingKind 换人时需要撤销的待处理项种类（§5.8）。
type PendingKind string

const (
	PendingDelivery PendingKind = "delivery" // 尚未送达某 bot 的投递
	PendingApproval PendingKind = "approval" // 等待某 bot 处理的审批
)

// Pending 一条尚未产生外部副作用的投递或审批。
//
// 撤销只对"还没发生的动作"有效：已经发进客户端、已经开始执行的动作平台无法回滚，
// 只能标记出来告诉用户（见 README「方案内部的矛盾」第 3 条）。
type Pending struct {
	ID     string      `json:"id"`
	WorkID string      `json:"work_id"`
	BotID  string      `json:"bot_id"`
	Kind   PendingKind `json:"kind"`
}

// Store 编排层依赖的持久化接口。
//
// 编排层不写 SQL、不 import store 包的具体实现；store 包实现本接口后由 app 层注入。
// 方法只覆盖编排层真正需要的东西，多一个都不要加（§11「过度设计」）。
type Store interface {
	SaveWork(ctx context.Context, w contract.Work) error
	GetWork(ctx context.Context, workID string) (contract.Work, error)
	ListWorks(ctx context.Context, roomID string, states []contract.WorkState) ([]contract.Work, error)

	SaveMessage(ctx context.Context, m contract.Message) error
	GetMessage(ctx context.Context, messageID string) (contract.Message, error)

	SaveReview(ctx context.Context, r contract.Review) error
	Reviews(ctx context.Context, workID string) ([]contract.Review, error)

	SaveAssignment(ctx context.Context, workID string, a contract.RoleAssignment) error
	Assignments(ctx context.Context, workID string) ([]contract.RoleAssignment, error)

	AppendEvent(ctx context.Context, e contract.Event) error

	PendingFor(ctx context.Context, workID string) ([]Pending, error)
	RevokePending(ctx context.Context, pendingID, reason string) error

	// MarkInputSeen 幂等登记 (message_id, bot_id)；已存在返回 seen=true。
	// 必须是原子操作（唯一索引或事务），否则重启后会重复唤醒同一个 bot（§5.3 护栏一）。
	MarkInputSeen(ctx context.Context, messageID, botID string) (seen bool, err error)
}
