package contract

import "time"

// EventKind 归一化事件六类（§4.3）。适配器只能产出这六种。
type EventKind string

const (
	EventSession  EventKind = "session"
	EventProgress EventKind = "progress"
	EventOutput   EventKind = "output"
	EventApproval EventKind = "approval"
	EventFinal    EventKind = "final"
	EventError    EventKind = "error"
)

// SchemaVersion 当前事件信封版本，写进每条事件，便于旧数据重放。
const SchemaVersion = 1

// Event 引擎输出与平台存储之间唯一的归一化格式。
//
// Seq 在每个 Run 内单调递增，UI 断线后按游标补齐并去重。
// 流式 output 文本不得触发唤醒（§5.6）。
type Event struct {
	EventID       string    `json:"event_id"`
	WorkID        string    `json:"work_id"`
	RunID         string    `json:"run_id"`
	BotID         string    `json:"bot_id"`
	Seq           int64     `json:"seq"`
	TS            time.Time `json:"ts"`
	SchemaVersion int       `json:"schema_version"`
	Kind          EventKind `json:"kind"`

	// 以下按 Kind 择一填充。

	Session  *SessionInfo  `json:"session,omitempty"`  // kind=session
	Text     string        `json:"text,omitempty"`     // kind=output|progress|error
	Final    *FinalInfo    `json:"final,omitempty"`    // kind=final
	Approval *ApprovalInfo `json:"approval,omitempty"` // kind=approval
}

// SessionInfo 绑定到哪个客户端、哪个专用会话（§4.2.2）。
// ClientPID / WindowHandle 用于重新关联，不可用作鉴权依据。
type SessionInfo struct {
	Adapter     string `json:"adapter"`      // codex | dsh | trae | doubao
	ClientPID   int    `json:"client_pid"`
	WindowTitle string `json:"window_title,omitempty"`
	// NativeID 客户端自身的会话标识；取不到就留空并置 Recovered=false。
	NativeID  string `json:"native_id,omitempty"`
	Recovered bool   `json:"recovered"`
}

// FinalInfo 终结事件。
//
// Success 只能来自 SuccessSignal：客户端进程退出码不可信
// （已实测 DSH 模型调用失败仍可能 exit 0），禁止用退出码判定成功。
type FinalInfo struct {
	Success       bool          `json:"success"`
	SuccessSignal string        `json:"success_signal"`
	Summary       string        `json:"summary,omitempty"`
	Artifacts     []ArtifactRef `json:"artifacts,omitempty"`
	Usage         *Usage        `json:"usage,omitempty"`
	// Unverified 为 true 表示完成状态未能核实，UI 必须显示"未确认"。
	Unverified bool `json:"unverified,omitempty"`
}

// ApprovalInfo 客户端内部的授权请求（§9.3）。
// 平台不得自动点"全部允许"；语义与目标均可验证时才桥接，否则提示用户回到客户端决定。
type ApprovalInfo struct {
	ApprovalID string   `json:"approval_id"`
	Prompt     string   `json:"prompt"`
	Options    []string `json:"options,omitempty"`
	// Bridged=false 表示只能让用户去客户端处理，平台无权代答。
	Bridged bool `json:"bridged"`
}
