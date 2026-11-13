package contract

import "time"

// FromKind 消息来源种类。
type FromKind string

const (
	FromUser FromKind = "user"
	FromBot  FromKind = "bot"
	FromSystem FromKind = "system"
)

// MessageKind 消息语义类型（§5.1）。
// handoff 是关键语义：它把"某件事交给某人"显式化，而不是靠自然语言猜测。
type MessageKind string

const (
	MsgChat   MessageKind = "chat"
	MsgTask   MessageKind = "task"
	MsgResult MessageKind = "result"
	MsgHandoff MessageKind = "handoff"
	MsgNotice MessageKind = "notice"
)

// Message 聊天与协作消息的统一结构（§5.1）。
//
// ToID 不自动代表私密：它表达"这条消息在语义上指向谁"，可见性由 RoomID 决定。
// ReplyTo 构成因果链，用于交接深度上限（§5.3，深度 ≤6）。
type Message struct {
	ID        string       `json:"id"`
	RoomID    string       `json:"room_id"`
	ThreadID  string       `json:"thread_id,omitempty"`
	WorkID    string       `json:"work_id,omitempty"`
	CauseID   string       `json:"cause_id,omitempty"`
	FromID    string       `json:"from_id"`
	FromKind  FromKind     `json:"from_kind"`
	ToID      string       `json:"to_id,omitempty"`
	Kind      MessageKind  `json:"kind"`
	Text      string       `json:"text"`
	Mentions  []string     `json:"mentions,omitempty"`
	ReplyTo   string       `json:"reply_to,omitempty"`
	Files     []string     `json:"files,omitempty"`
	CreatedAt time.Time    `json:"created_at"`

	// ClientMessageID 用于幂等：同一条用户输入重投不得重复建 Work（§5.6）。
	ClientMessageID string `json:"client_message_id,omitempty"`
}
