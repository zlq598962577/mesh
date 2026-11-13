package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"mesh/internal/contract"
)

// 四个结构化协作工具（§5.5）。P0 只做这四个，不做自由协议。
const (
	ToolCollaborationSend    = "collaboration.send"    // 给某人发消息/请协助
	ToolCollaborationHandoff = "collaboration.handoff" // 把活交给某人，自己退出
	ToolWorkSubmit           = "work.submit"           // 执行者提交产物
	ToolReviewSubmit         = "review.submit"         // 审查者提交结论
)

// ToolError 工具调用失败。这类错误要回给模型让它重试，不能当成"通过"。
type ToolError struct {
	Tool   string
	Reason string
}

func (e *ToolError) Error() string { return fmt.Sprintf("%s: %s", e.Tool, e.Reason) }

// ---- collaboration.send ----

// sendArgs collaboration.send 的参数。
type sendArgs struct {
	To   string `json:"to"`
	Text string `json:"text"`
}

// parseSendArgs 解析并校验参数。
func parseSendArgs(raw string) (sendArgs, error) {
	var a sendArgs
	if err := strictUnmarshal(raw, &a); err != nil {
		return a, &ToolError{Tool: ToolCollaborationSend, Reason: "参数不是合法 JSON：" + err.Error()}
	}
	a.To = strings.TrimSpace(a.To)
	a.Text = strings.TrimSpace(a.Text)
	if a.To == "" {
		return a, &ToolError{Tool: ToolCollaborationSend, Reason: "缺少 to"}
	}
	if a.Text == "" {
		return a, &ToolError{Tool: ToolCollaborationSend, Reason: "缺少 text"}
	}
	return a, nil
}

// ---- collaboration.handoff ----

// handoffArgs collaboration.handoff 的参数。
type handoffArgs struct {
	To     string `json:"to"`
	Text   string `json:"text"`
	Reason string `json:"reason"`
}

func parseHandoffArgs(raw string) (handoffArgs, error) {
	var a handoffArgs
	if err := strictUnmarshal(raw, &a); err != nil {
		return a, &ToolError{Tool: ToolCollaborationHandoff, Reason: "参数不是合法 JSON：" + err.Error()}
	}
	a.To = strings.TrimSpace(a.To)
	a.Text = strings.TrimSpace(a.Text)
	if a.To == "" {
		return a, &ToolError{Tool: ToolCollaborationHandoff, Reason: "缺少 to"}
	}
	if a.Text == "" {
		return a, &ToolError{Tool: ToolCollaborationHandoff, Reason: "缺少 text"}
	}
	return a, nil
}

// ---- work.submit ----

// submitArgs work.submit 的参数。
//
// Version 由平台分配，模型不能自己填：否则模型可以回填一个旧版本号来"复活"
// 已被否掉的版本。这里明确忽略任何模型传入的 version 字段。
type submitArgs struct {
	Summary   string   `json:"summary"`
	Kind      string   `json:"kind"`
	Ref       string   `json:"ref"`
	Files     []string `json:"files"`
	SelfCheck string   `json:"self_check"`
}

func parseSubmitArgs(raw string) (submitArgs, error) {
	var a submitArgs
	if err := strictUnmarshal(raw, &a); err != nil {
		return a, &ToolError{Tool: ToolWorkSubmit, Reason: "参数不是合法 JSON：" + err.Error()}
	}
	a.Summary = strings.TrimSpace(a.Summary)
	a.Kind = strings.TrimSpace(a.Kind)
	a.Ref = strings.TrimSpace(a.Ref)
	if a.Summary == "" {
		return a, &ToolError{Tool: ToolWorkSubmit, Reason: "缺少 summary（必须说明这次交付了什么）"}
	}
	if a.Kind == "" {
		return a, &ToolError{Tool: ToolWorkSubmit, Reason: "缺少 kind"}
	}
	if a.Ref == "" && len(a.Files) == 0 {
		return a, &ToolError{Tool: ToolWorkSubmit, Reason: "缺少 ref 或 files：产物必须能定位"}
	}
	return a, nil
}

// ---- review.submit ----

// reviewArgs review.submit 的参数。
type reviewArgs struct {
	Verdict  string `json:"verdict"`
	Findings string `json:"findings"`
	// ArtifactVersion 审查针对的版本号，必须回填，防止审"最新"却批了旧版本。
	ArtifactVersion int `json:"artifact_version"`
}

// parseReviewArgs 解析审查结论。
//
// 关键约束：解析失败、结论缺失、结论拼错，一律不返回 approved。
// 返回 Verdict 为空 + error，由调用方转 waiting_user 并告知用户"审查结论没读懂"。
func parseReviewArgs(raw string) (reviewArgs, error) {
	var a reviewArgs
	if err := strictUnmarshal(raw, &a); err != nil {
		return reviewArgs{}, &ToolError{Tool: ToolReviewSubmit, Reason: "参数不是合法 JSON：" + err.Error()}
	}
	a.Verdict = strings.TrimSpace(strings.ToLower(a.Verdict))
	a.Findings = strings.TrimSpace(a.Findings)

	switch a.Verdict {
	case string(contract.VerdictApproved):
		if a.ArtifactVersion <= 0 {
			return reviewArgs{}, &ToolError{Tool: ToolReviewSubmit,
				Reason: "approved 必须带 artifact_version，否则无法确认批的是哪个版本"}
		}
		if a.Findings == "" {
			return reviewArgs{}, &ToolError{Tool: ToolReviewSubmit,
				Reason: "approved 也要写 findings（至少一句你验了什么）"}
		}
		return a, nil
	case string(contract.VerdictChangesRequested):
		if a.Findings == "" {
			return reviewArgs{}, &ToolError{Tool: ToolReviewSubmit,
				Reason: "changes_requested 必须写 findings，说明要改什么"}
		}
		return a, nil
	case string(contract.VerdictBlocked):
		if a.Findings == "" {
			return reviewArgs{}, &ToolError{Tool: ToolReviewSubmit,
				Reason: "blocked 必须写 findings，说明卡在哪"}
		}
		return a, nil
	default:
		// 空、拼错、自造结论（如 "lgtm" "pass" "ok"）都走这里。
		// 绝不默认 approved。
		return reviewArgs{}, &ToolError{Tool: ToolReviewSubmit,
			Reason: fmt.Sprintf("结论 %q 不是 approved / changes_requested / blocked 之一", a.Verdict)}
	}
}

// strictUnmarshal 严格 JSON 解析：拒绝未知字段，防止模型塞 version 之类的越权字段。
func strictUnmarshal(raw string, v any) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("参数为空")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	// 拒绝 JSON 之后还有多余内容（模型常见的手抖）。
	if dec.More() {
		return errors.New("JSON 之后还有多余内容")
	}
	return nil
}

// ---- 落地执行 ----

// ToolResult 一次工具调用的结果，回给调用方（引擎/UI）。
type ToolResult struct {
	OK      bool
	Text    string
	Notice  string // 需要告知用户的提示，空表示无
	Verdict contract.Verdict
}

// CallTool 分派四个工具。工具名未知返回错误，不做"猜一个"。
func (o *Orchestrator) CallTool(ctx context.Context, botID, workID, name, rawArgs string) (ToolResult, error) {
	switch name {
	case ToolCollaborationSend:
		return o.toolSend(ctx, botID, workID, rawArgs)
	case ToolCollaborationHandoff:
		return o.toolHandoff(ctx, botID, workID, rawArgs)
	case ToolWorkSubmit:
		return o.toolWorkSubmit(ctx, botID, workID, rawArgs)
	case ToolReviewSubmit:
		return o.toolReviewSubmit(ctx, botID, workID, rawArgs)
	default:
		return ToolResult{}, &ToolError{Tool: name, Reason: "未知工具，只支持 collaboration.send / collaboration.handoff / work.submit / review.submit"}
	}
}

func (o *Orchestrator) toolSend(ctx context.Context, botID, workID, raw string) (ToolResult, error) {
	a, err := parseSendArgs(raw)
	if err != nil {
		return ToolResult{}, err
	}
	if !o.knownBot(a.To) {
		return ToolResult{}, &ToolError{Tool: ToolCollaborationSend, Reason: fmt.Sprintf("对象 %s 不在本房间", a.To)}
	}
	m := contract.Message{
		ID: o.newID("msg"), WorkID: workID, FromID: botID, FromKind: contract.FromBot,
		ToID: a.To, Kind: contract.MsgChat, Text: a.Text, CreatedAt: o.now(),
	}
	if err := o.store.SaveMessage(ctx, m); err != nil {
		return ToolResult{}, err
	}
	return ToolResult{OK: true, Text: "已发送给 " + a.To}, nil
}

func (o *Orchestrator) toolHandoff(ctx context.Context, botID, workID, raw string) (ToolResult, error) {
	a, err := parseHandoffArgs(raw)
	if err != nil {
		return ToolResult{}, err
	}
	if !o.knownBot(a.To) {
		return ToolResult{}, &ToolError{Tool: ToolCollaborationHandoff, Reason: fmt.Sprintf("对象 %s 不在本房间", a.To)}
	}
	if a.To == botID {
		return ToolResult{}, &ToolError{Tool: ToolCollaborationHandoff, Reason: "不能把活交接给自己"}
	}
	w, err := o.store.GetWork(ctx, workID)
	if err != nil {
		return ToolResult{}, err
	}
	// 交接会改分工：执行者换成 to，来源记为 SoulSuggest（bot 自己提的，优先级最低）。
	// 若用户此前已明确指定过执行者，这次交接会被 resolveAssignment 忽略——
	// 这是有意的：bot 不能推翻用户的明确指定。
	revoked, err := o.changeAssignment(ctx, &w, contract.RoleAssignment{
		Executor: a.To, Reviewer: w.Assign.Reviewer, Helpers: w.Assign.Helpers,
		Source: contract.SourceSoulSuggest,
	})
	if err != nil {
		var re *RoleError
		if errors.As(err, &re) {
			return ToolResult{OK: false, Notice: re.Reason}, nil
		}
		return ToolResult{}, err
	}
	m := contract.Message{
		ID: o.newID("msg"), WorkID: workID, FromID: botID, FromKind: contract.FromBot,
		ToID: a.To, Kind: contract.MsgHandoff, Text: a.Text, CreatedAt: o.now(),
	}
	if err := o.store.SaveMessage(ctx, m); err != nil {
		return ToolResult{}, err
	}
	res := ToolResult{OK: true, Text: "已交接给 " + a.To}
	if len(revoked) > 0 {
		res.Notice = fmt.Sprintf("已撤销 %d 条发给原执行者的待处理项", len(revoked))
	}
	return res, nil
}

func (o *Orchestrator) toolWorkSubmit(ctx context.Context, botID, workID, raw string) (ToolResult, error) {
	a, err := parseSubmitArgs(raw)
	if err != nil {
		return ToolResult{}, err
	}
	w, err := o.store.GetWork(ctx, workID)
	if err != nil {
		return ToolResult{}, err
	}
	if w.Assign.Executor != botID {
		return ToolResult{}, &ToolError{Tool: ToolWorkSubmit,
			Reason: fmt.Sprintf("只有执行者 %s 能提交产物，你是 %s", w.Assign.Executor, botID)}
	}
	if w.State != contract.WorkExecuting {
		return ToolResult{}, &ToolError{Tool: ToolWorkSubmit,
			Reason: fmt.Sprintf("当前状态 %s 不能提交产物", w.State)}
	}

	// 版本号由平台分配：只增不减，模型无法回填。
	ver := latestVersion(&w) + 1
	art := contract.ArtifactRef{
		WorkID: workID, Version: ver, Kind: a.Kind, Ref: a.Ref,
		Summary: a.Summary, Author: botID,
	}
	w.Artifacts = append(w.Artifacts, art)

	hasReviewer := requiresReview(w.Assign)
	if err := onArtifactSubmitted(&w, art, o.now()); err != nil {
		return ToolResult{}, err
	}
	if err := o.store.SaveWork(ctx, w); err != nil {
		return ToolResult{}, err
	}

	res := ToolResult{OK: true, Text: fmt.Sprintf("已提交 v%d", ver)}
	if hasReviewer {
		res.Text += "，等待 " + w.Assign.Reviewer + " 审查"
	} else {
		res.Text += "，本委托未配置审查者，已直接完成"
		res.Notice = "这个委托没有独立审查者，产物未经审查即为最终结果"
	}
	return res, nil
}

func (o *Orchestrator) toolReviewSubmit(ctx context.Context, botID, workID, raw string) (ToolResult, error) {
	a, err := parseReviewArgs(raw)
	if err != nil {
		return ToolResult{}, err
	}
	w, err := o.store.GetWork(ctx, workID)
	if err != nil {
		return ToolResult{}, err
	}
	if w.Assign.Reviewer != botID {
		return ToolResult{}, &ToolError{Tool: ToolReviewSubmit,
			Reason: fmt.Sprintf("只有审查者 %s 能提交结论，你是 %s", w.Assign.Reviewer, botID)}
	}

	r := contract.Review{
		ID: o.newID("rev"), WorkID: workID, ArtifactVersion: a.ArtifactVersion,
		Reviewer: botID, Verdict: contract.Verdict(a.Verdict),
		Findings: a.Findings, CreatedAt: o.now(),
	}
	if err := o.store.SaveReview(ctx, r); err != nil {
		return ToolResult{}, err
	}
	if err := onReviewVerdict(&w, r, o.now()); err != nil {
		var we *WorkError
		if errors.As(err, &we) {
			// 旧版本获批等情况：记录已存，状态不动，如实告知。
			if err := o.store.SaveWork(ctx, w); err != nil {
				return ToolResult{}, err
			}
			return ToolResult{OK: false, Verdict: r.Verdict, Notice: we.Reason}, nil
		}
		return ToolResult{}, err
	}
	if err := o.store.SaveWork(ctx, w); err != nil {
		return ToolResult{}, err
	}

	res := ToolResult{OK: true, Verdict: r.Verdict}
	switch r.Verdict {
	case contract.VerdictApproved:
		res.Text = fmt.Sprintf("v%d 已通过审查，委托完成", r.ArtifactVersion)
	case contract.VerdictChangesRequested:
		res.Text = fmt.Sprintf("v%d 要求修改，已退回执行者（第 %d/%d 轮）",
			r.ArtifactVersion, w.ReviewRounds, contract.MaxAutoReviewRounds)
		if w.State == contract.WorkWaitingUser {
			res.Notice = fmt.Sprintf("自动修改已到 %d 轮上限，需要你决定是否继续", contract.MaxAutoReviewRounds)
		}
	case contract.VerdictBlocked:
		res.Text = "审查判定受阻，已交回用户"
		res.Notice = "审查者认为当前无法继续，请补充信息或换人"
	}
	return res, nil
}
