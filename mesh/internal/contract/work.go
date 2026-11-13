package contract

import "time"

// Role 一次 Work 内的角色。是"这件事里谁干什么"，不是固定职称（§5.7）。
type Role string

const (
	RoleExecutor Role = "executor"
	RoleReviewer Role = "reviewer"
	RoleHelper   Role = "helper"
)

// RoleSource 分工来源，优先级从高到低（§12）。
// 用户对当前委托的最新明确指定 > 发起时的界面选择 > 房间默认 > agent 的 soul 建议。
type RoleSource string

const (
	SourceUserChat   RoleSource = "user_chat"    // 最高
	SourceUserUI     RoleSource = "user_ui"
	SourceRoomDefault RoleSource = "room_default"
	SourceSoulSuggest RoleSource = "soul_suggest" // 最低，仅建议
)

// RoleAssignment 一次生效的分工。Version 单调递增，
// 换人时递增并撤销旧角色尚未处理的投递与审批（§5.8）。
type RoleAssignment struct {
	Version  int        `json:"version"`
	Executor string     `json:"executor"`
	Reviewer string     `json:"reviewer"` // 空 = 用户明确选择不审查
	Helpers  []string   `json:"helpers,omitempty"`
	Source   RoleSource `json:"source"`
	At       time.Time  `json:"at"`
}

// WorkState 委托状态机（§5.7）。
//
// queued → executing → reviewing → completed
// reviewing → executing  （要求修改）
// 无审查者时 executing → completed
// 自动修改最多 2 轮，仍不通过转 waiting_user。
type WorkState string

const (
	WorkQueued      WorkState = "queued"
	WorkExecuting   WorkState = "executing"
	WorkReviewing   WorkState = "reviewing"
	WorkPaused      WorkState = "paused"
	WorkWaitingUser WorkState = "waiting_user"
	WorkCompleted   WorkState = "completed"
	WorkFailed      WorkState = "failed"
	WorkCancelled   WorkState = "cancelled"
)

// MaxAutoReviewRounds 自动审查修改轮次上限（§5.7 / §12 默认 2）。
const MaxAutoReviewRounds = 2

// Verdict 审查结论（§5.5）。
// 解析失败一律不得自动判为 approved。
type Verdict string

const (
	VerdictApproved         Verdict = "approved"
	VerdictChangesRequested Verdict = "changes_requested"
	VerdictBlocked          Verdict = "blocked"
)

// Review 一次针对特定产物版本的审查（§2）。
// 完成判定绑定 Version：旧版本的 approved 不能让新版本通过。
type Review struct {
	ID             string    `json:"id"`
	WorkID         string    `json:"work_id"`
	ArtifactVersion int      `json:"artifact_version"`
	Reviewer       string    `json:"reviewer"`
	Verdict        Verdict   `json:"verdict"`
	Findings       string    `json:"findings,omitempty"`
	// Skipped 表示用户明确跳过审查，只能标注"未审查/用户接受"，不得伪造通过。
	Skipped   bool      `json:"skipped,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Work 用户的一次委托（§2）。与 Run、Review 三者不可混为一谈：
//   - Work   = 这件事办完了没有
//   - Run    = 某一次执行结束了没有
//   - Review = 某个产物版本通过了没有
type Work struct {
	ID        string `json:"id"`
	RoomID    string `json:"room_id"`
	ThreadID  string `json:"thread_id,omitempty"`
	Goal      string `json:"goal"`
	State     WorkState `json:"state"`
	Assign    RoleAssignment `json:"assign"`
	// Artifacts 按 Version 递增保存全部历史版本，审查必须能引用旧版本。
	Artifacts []ArtifactRef `json:"artifacts,omitempty"`
	Final     *ArtifactRef  `json:"final,omitempty"`
	// ReviewRounds 已发生的自动审查修改轮次。
	ReviewRounds int `json:"review_rounds"`
	// Budget 本次委托的预算覆盖（房间默认之上）。
	Budget    *Budget   `json:"budget,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
