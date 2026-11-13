package contract

import "context"

// Session 一个已打开的客户端会话句柄。
type Session interface {
	ID() string
}

// Turn 一次发送产生的执行轮次。
type Turn interface {
	ID() string
}

// Decision 审批决定。
type Decision string

const (
	DecisionAllow       Decision = "allow"
	DecisionAllowOnce   Decision = "allow_once"
	DecisionDeny        Decision = "deny"
	DecisionUnavailable Decision = "unavailable" // 平台无权代答，交回用户
)

// Engine 统一引擎契约（§4.2）。
//
// 四个桌面客户端各自实现本接口；平台其余部分只认这个接口，不认具体客户端。
// 实现方必须保证：
//   - Events 关闭代表该会话不再产生事件，不代表成功。
//   - Cancel 不得通过杀掉整个客户端来实现（§4.2.2）。
//   - Close/Suspend 只释放平台持有的连接，不关闭用户自己的窗口。
//   - 平台退出时不把用户已有客户端加入 Job Object 的 kill-on-close 名单（§9.2）。
type Engine interface {
	Open(ctx context.Context, spec BotSpec, ws Workspace) (Session, error)
	Send(ctx context.Context, s Session, msg Message) (Turn, error)
	Events(s Session) <-chan Event
	Cancel(ctx context.Context, s Session, turnID string) error
	ResolveApproval(ctx context.Context, s Session, approvalID string, decision Decision) error
	Suspend(s Session) error
	Close(s Session) error
	Health() error
}
