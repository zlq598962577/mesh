// Package contract 是本项目唯一的跨模块共享类型层。
//
// 规则（Lead 冻结，勿改）：
//  1. 任何跨模块传递的类型必须定义在本包，其它包不得自建同名结构体。
//  2. 本包只依赖标准库，不得引入任何第三方依赖。
//  3. 字段的 JSON/YAML 名字与《多Agent协作平台技术方案 v4》一致，改动等于改协议。
package contract

// ArtifactRef 指向某个 Work 的某个产物版本（§8.4）。
// 审查者看到的必须是同一个 Version 的只读快照，不能是"最新"。
type ArtifactRef struct {
	WorkID  string `json:"work_id"`
	Version int    `json:"version"`
	Kind    string `json:"kind"` // text | patch | commit | file | url
	Ref     string `json:"ref"`  // commit sha / 相对路径 / URL
	Summary string `json:"summary,omitempty"`
	Author  string `json:"author,omitempty"` // bot id
}

// Usage 一次 Run 的用量（§9.6）。
//
// Known=false 表示该通道无法可靠计量（四个桌面客户端首版默认就是这种情况）。
// 未知必须如实呈现为未知，绝不允许记成 0，也绝不允许据此宣称金额硬顶生效。
type Usage struct {
	Known           bool   `json:"known"`
	Provider        string `json:"provider,omitempty"`
	Model           string `json:"model,omitempty"`
	TokensIn        int64  `json:"tokens_in,omitempty"`
	TokensOut       int64  `json:"tokens_out,omitempty"`
	CacheReadTokens int64  `json:"cache_read_tokens,omitempty"`
	Currency        string `json:"currency,omitempty"`
	AmountMicros    int64  `json:"amount_micros,omitempty"`
	PriceVersion    string `json:"price_version,omitempty"`
	// Note 说明为何未知或为估算，UI 直接展示给用户。
	Note string `json:"note,omitempty"`
}

// OperationState 外部副作用的四态（§5.8）。
// 平台无法验证客户端内部动作是否停止时，只能是 unknown，不能猜。
type OperationState string

const (
	OpPrepared  OperationState = "prepared"
	OpExecuting OperationState = "executing"
	OpSucceeded OperationState = "succeeded"
	OpFailed    OperationState = "failed"
	OpUnknown   OperationState = "unknown"
)
