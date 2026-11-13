package orchestrator

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"mesh/internal/contract"
)

// 小工具：避免测试里到处写 errors.As。
func asWorkError(err error, target **WorkError) bool { return errors.As(err, target) }
func asRoleError(err error, target **RoleError) bool { return errors.As(err, target) }
func itoa(i int) string                              { return strconv.Itoa(i) }

// bg 用 context.Background 而非 t.Context：go.mod 声明 go 1.23，t.Context 是 1.24 才有的。
func bg() context.Context { return context.Background() }

// 用例全部在 Go 1.27.4 windows/amd64 上跑过：56 个全绿。
// 断言一律读 fakeStore 里的状态，不读本地 w —— OnUserMessage 改的是从
// GetWork 取回的那份副本，本地 w 永远不变，拿它断言等于什么都没测。
// 写成表驱动，便于后续改动时一眼核对预期。

func at(sec int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, sec, 0, time.UTC)
}

func baseWork(assign contract.RoleAssignment) contract.Work {
	w := newWork("w1", "r1", "t1", "把方案写出来", assign, nil, at(0))
	return w
}

func withReviewer() contract.RoleAssignment {
	return contract.RoleAssignment{Version: 1, Executor: "codex", Reviewer: "trae", Source: contract.SourceUserChat}
}

func withoutReviewer() contract.RoleAssignment {
	return contract.RoleAssignment{Version: 1, Executor: "codex", Source: contract.SourceUserChat}
}

// TestWorkHappyPath 正常路径：queued → executing → reviewing → completed。
func TestWorkHappyPath(t *testing.T) {
	w := baseWork(withReviewer())

	if err := transition(&w, contract.WorkExecuting, at(1)); err != nil {
		t.Fatalf("queued→executing 应允许，得到 %v", err)
	}
	art := contract.ArtifactRef{WorkID: w.ID, Version: 1, Kind: "file", Ref: "out.md", Author: "codex"}
	w.Artifacts = append(w.Artifacts, art)
	if err := onArtifactSubmitted(&w, art, at(2)); err != nil {
		t.Fatalf("提交产物应进入 reviewing，得到 %v", err)
	}
	if w.State != contract.WorkReviewing {
		t.Fatalf("期望 reviewing，得到 %s", w.State)
	}
	rev := contract.Review{ID: "rv1", WorkID: w.ID, ArtifactVersion: 1,
		Reviewer: "trae", Verdict: contract.VerdictApproved, Findings: "验过"}
	if err := onReviewVerdict(&w, rev, at(3)); err != nil {
		t.Fatalf("approved 应完成，得到 %v", err)
	}
	if w.State != contract.WorkCompleted {
		t.Fatalf("期望 completed，得到 %s", w.State)
	}
	if w.Final == nil || w.Final.Version != 1 {
		t.Fatalf("Final 应绑定 v1，得到 %+v", w.Final)
	}
}

// TestNoReviewerCompletesDirectly 无审查者：executing → completed，不进入 reviewing。
func TestNoReviewerCompletesDirectly(t *testing.T) {
	w := baseWork(withoutReviewer())
	if err := transition(&w, contract.WorkExecuting, at(1)); err != nil {
		t.Fatal(err)
	}
	art := contract.ArtifactRef{WorkID: w.ID, Version: 1, Kind: "file", Ref: "out.md"}
	w.Artifacts = append(w.Artifacts, art)
	if err := onArtifactSubmitted(&w, art, at(2)); err != nil {
		t.Fatal(err)
	}
	if w.State != contract.WorkCompleted {
		t.Fatalf("无审查者应直接 completed，得到 %s", w.State)
	}
}

// TestOldVersionApprovalCannotReleaseNewer 关键不变量：
// 旧版本的 approved 不得放行更新的版本（§11 风险「旧版本获批」）。
func TestOldVersionApprovalCannotReleaseNewer(t *testing.T) {
	w := baseWork(withReviewer())
	w.State = contract.WorkReviewing
	// 已有 v1、v2 两个版本（v2 是执行者根据第一轮意见改的）
	w.Artifacts = []contract.ArtifactRef{
		{WorkID: w.ID, Version: 1, Kind: "file", Ref: "v1.md"},
		{WorkID: w.ID, Version: 2, Kind: "file", Ref: "v2.md"},
	}

	// 审查者对 v1 给出 approved（迟到的旧审批）
	rev := contract.Review{ID: "rv1", WorkID: w.ID, ArtifactVersion: 1,
		Reviewer: "trae", Verdict: contract.VerdictApproved, Findings: "v1 没问题"}
	err := onReviewVerdict(&w, rev, at(3))
	if err == nil {
		t.Fatal("旧版本 approved 必须被拒绝，但返回了 nil")
	}
	var we *WorkError
	if !asWorkError(err, &we) {
		t.Fatalf("期望 *WorkError，得到 %T", err)
	}
	if w.State != contract.WorkReviewing {
		t.Fatalf("旧审批不得改变状态，期望 reviewing，得到 %s", w.State)
	}
	if w.Final != nil {
		t.Fatalf("旧审批不得设置 Final，得到 %+v", w.Final)
	}

	// 对 v2 的 approved 才生效
	rev2 := contract.Review{ID: "rv2", WorkID: w.ID, ArtifactVersion: 2,
		Reviewer: "trae", Verdict: contract.VerdictApproved, Findings: "v2 验过"}
	if err := onReviewVerdict(&w, rev2, at(4)); err != nil {
		t.Fatalf("最新版本 approved 应完成，得到 %v", err)
	}
	if w.State != contract.WorkCompleted {
		t.Fatalf("期望 completed，得到 %s", w.State)
	}
	if w.Final == nil || w.Final.Version != 2 {
		t.Fatalf("Final 应绑定 v2，得到 %+v", w.Final)
	}
}

// TestApprovalForNonexistentVersion 指向不存在的版本不得完成。
func TestApprovalForNonexistentVersion(t *testing.T) {
	w := baseWork(withReviewer())
	w.State = contract.WorkReviewing
	w.Artifacts = []contract.ArtifactRef{{WorkID: w.ID, Version: 1, Kind: "file", Ref: "v1.md"}}

	rev := contract.Review{ID: "rv", WorkID: w.ID, ArtifactVersion: 9,
		Reviewer: "trae", Verdict: contract.VerdictApproved, Findings: "x"}
	if err := onReviewVerdict(&w, rev, at(3)); err == nil {
		t.Fatal("指向不存在版本的 approved 必须被拒绝")
	}
	if w.State == contract.WorkCompleted {
		t.Fatal("不得完成")
	}
}

// TestAutoRevisionCap 自动修改最多 2 轮，第 3 次要求修改转 waiting_user。
func TestAutoRevisionCap(t *testing.T) {
	w := baseWork(withReviewer())
	w.State = contract.WorkReviewing

	for i := 1; i <= contract.MaxAutoReviewRounds; i++ {
		rev := contract.Review{ID: "rv", WorkID: w.ID, ArtifactVersion: 1,
			Reviewer: "trae", Verdict: contract.VerdictChangesRequested, Findings: "再改"}
		if err := onReviewVerdict(&w, rev, at(i)); err != nil {
			t.Fatalf("第 %d 轮要求修改应回到 executing，得到 %v", i, err)
		}
		if w.State != contract.WorkExecuting {
			t.Fatalf("第 %d 轮期望 executing，得到 %s", i, w.State)
		}
		if w.ReviewRounds != i {
			t.Fatalf("期望 ReviewRounds=%d，得到 %d", i, w.ReviewRounds)
		}
		// 执行者改完再提交，回到 reviewing
		w.State = contract.WorkExecuting
		art := contract.ArtifactRef{WorkID: w.ID, Version: i + 1, Kind: "file", Ref: "v.md"}
		w.Artifacts = append(w.Artifacts, art)
		if err := onArtifactSubmitted(&w, art, at(10+i)); err != nil {
			t.Fatal(err)
		}
	}

	// 第 3 次要求修改：超限 → waiting_user
	rev := contract.Review{ID: "rv", WorkID: w.ID, ArtifactVersion: latestVersion(&w),
		Reviewer: "trae", Verdict: contract.VerdictChangesRequested, Findings: "还不行"}
	if err := onReviewVerdict(&w, rev, at(20)); err != nil {
		t.Fatalf("超限应迁移到 waiting_user 且不报错，得到 %v", err)
	}
	if w.State != contract.WorkWaitingUser {
		t.Fatalf("超限后期望 waiting_user，得到 %s", w.State)
	}
	if !maxAutoRoundsExceeded(&w) {
		t.Fatal("maxAutoRoundsExceeded 应为 true")
	}
}

// TestBlockedGoesToUser blocked 交回用户。
func TestBlockedGoesToUser(t *testing.T) {
	w := baseWork(withReviewer())
	w.State = contract.WorkReviewing
	rev := contract.Review{ID: "rv", WorkID: w.ID, ArtifactVersion: 1,
		Reviewer: "trae", Verdict: contract.VerdictBlocked, Findings: "缺资料"}
	if err := onReviewVerdict(&w, rev, at(1)); err != nil {
		t.Fatal(err)
	}
	if w.State != contract.WorkWaitingUser {
		t.Fatalf("期望 waiting_user，得到 %s", w.State)
	}
}

// TestUnknownVerdictNeverApproves 未知/解析失败的结论绝不自动通过。
func TestUnknownVerdictNeverApproves(t *testing.T) {
	for _, v := range []string{"", "lgtm", "pass", "ok", "APPROVED "} {
		w := baseWork(withReviewer())
		w.State = contract.WorkReviewing
		w.Artifacts = []contract.ArtifactRef{{WorkID: w.ID, Version: 1, Kind: "file", Ref: "v1.md"}}
		rev := contract.Review{ID: "rv", WorkID: w.ID, ArtifactVersion: 1,
			Reviewer: "trae", Verdict: contract.Verdict(v), Findings: "?"}
		_ = onReviewVerdict(&w, rev, at(1))
		if w.State == contract.WorkCompleted {
			t.Fatalf("结论 %q 不得完成 Work", v)
		}
		if w.Final != nil {
			t.Fatalf("结论 %q 不得设置 Final", v)
		}
	}
}

// TestIllegalTransitionRejected 非法迁移必须报错，不能静默。
func TestIllegalTransitionRejected(t *testing.T) {
	w := baseWork(withReviewer())
	// queued 不能直接到 reviewing
	if err := transition(&w, contract.WorkReviewing, at(1)); err == nil {
		t.Fatal("queued→reviewing 应被拒绝")
	}
	if w.State != contract.WorkQueued {
		t.Fatalf("拒绝后状态不应改变，得到 %s", w.State)
	}
	// 终态不可再迁移
	w.State = contract.WorkCompleted
	if err := transition(&w, contract.WorkExecuting, at(2)); err == nil {
		t.Fatal("completed→executing 应被拒绝")
	}
	// 幂等：同状态不报错
	if err := transition(&w, contract.WorkCompleted, at(3)); err != nil {
		t.Fatalf("同状态迁移应幂等，得到 %v", err)
	}
}

// TestCancelledIsTerminal cancelled 是终态。
func TestCancelledIsTerminal(t *testing.T) {
	w := baseWork(withReviewer())
	w.State = contract.WorkCancelled
	for _, to := range []contract.WorkState{
		contract.WorkQueued, contract.WorkExecuting, contract.WorkReviewing, contract.WorkCompleted,
	} {
		if canTransition(contract.WorkCancelled, to) {
			t.Fatalf("cancelled→%s 不应允许", to)
		}
	}
}

// TestAllStatesReachable 每个状态都能从 queued 走到（避免出现不可达状态）。
func TestAllStatesReachable(t *testing.T) {
	reach := map[contract.WorkState]bool{contract.WorkQueued: true}
	// 简单闭包传播
	for changed := true; changed; {
		changed = false
		for from, tos := range workStateTransitions {
			if !reach[from] {
				continue
			}
			for to := range tos {
				if !reach[to] {
					reach[to] = true
					changed = true
				}
			}
		}
	}
	all := []contract.WorkState{
		contract.WorkQueued, contract.WorkExecuting, contract.WorkReviewing,
		contract.WorkPaused, contract.WorkWaitingUser, contract.WorkCompleted,
		contract.WorkFailed, contract.WorkCancelled,
	}
	for _, s := range all {
		if !reach[s] {
			t.Fatalf("状态 %s 从 queued 不可达", s)
		}
	}
}

// TestNoTransitionToSelf 迁移表里不应出现自环（除终态外）。
func TestNoTransitionToSelf(t *testing.T) {
	for from, tos := range workStateTransitions {
		if tos[from] {
			t.Fatalf("状态 %s 的迁移表里出现了自环", from)
		}
	}
}

// ---- 分工优先级（§12） ----

// TestAssignmentPriority 高优先级覆盖低优先级，低优先级不得覆盖高优先级。
func TestAssignmentPriority(t *testing.T) {
	// 房间默认
	prev := contract.RoleAssignment{Version: 1, Executor: "codex", Reviewer: "trae", Source: contract.SourceRoomDefault}

	// soul 建议（更低）→ 忽略
	next, changed, err := resolveAssignment(&prev, contract.RoleAssignment{
		Executor: "doubao", Reviewer: "trae", Source: contract.SourceSoulSuggest})
	if err != nil {
		t.Fatal(err)
	}
	if changed || next.Executor != "codex" {
		t.Fatalf("soul 建议不得覆盖房间默认，得到 %+v changed=%v", next, changed)
	}

	// UI 选择（更高）→ 生效
	next, changed, err = resolveAssignment(&prev, contract.RoleAssignment{
		Executor: "doubao", Reviewer: "trae", Source: contract.SourceUserUI})
	if err != nil {
		t.Fatal(err)
	}
	if !changed || next.Executor != "doubao" {
		t.Fatalf("UI 应覆盖房间默认，得到 %+v changed=%v", next, changed)
	}
	if next.Version != 2 {
		t.Fatalf("版本应递增到 2，得到 %d", next.Version)
	}

	// 用户聊天（最高）→ 生效
	next, changed, err = resolveAssignment(&next, contract.RoleAssignment{
		Executor: "doubao", Reviewer: "dsh", Source: contract.SourceUserChat})
	if err != nil {
		t.Fatal(err)
	}
	if !changed || next.Reviewer != "dsh" {
		t.Fatalf("聊天应覆盖 UI，得到 %+v", next)
	}
	if next.Version != 3 {
		t.Fatalf("版本应递增到 3，得到 %d", next.Version)
	}
}

// TestAssignmentUnknownSourceRejected 未知来源不猜。
func TestAssignmentUnknownSourceRejected(t *testing.T) {
	prev := contract.RoleAssignment{Version: 1, Executor: "codex", Source: contract.SourceUserChat}
	if _, _, err := resolveAssignment(&prev, contract.RoleAssignment{
		Executor: "trae", Source: contract.RoleSource("mystery")}); err == nil {
		t.Fatal("未知来源应被拒绝")
	}
}

// TestAssignmentSamePeopleKeepsPending 同级重复指定同一批人不算换人（不撤销待处理项）。
func TestAssignmentSamePeopleKeepsPending(t *testing.T) {
	prev := contract.RoleAssignment{Version: 1, Executor: "codex", Reviewer: "trae", Source: contract.SourceUserChat}
	next, changed, err := resolveAssignment(&prev, contract.RoleAssignment{
		Executor: "codex", Reviewer: "trae", Source: contract.SourceUserChat})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("人没变不应算作分工变更")
	}
	if next.Version != 2 {
		t.Fatalf("仍应记一条新版本，得到 %d", next.Version)
	}
}

// TestSelfReviewRejected 执行者==审查者必须被拒（不得把自审包装成独立审查）。
func TestSelfReviewRejected(t *testing.T) {
	err := validateAssignment(contract.RoleAssignment{Executor: "codex", Reviewer: "codex"})
	if err == nil {
		t.Fatal("自审应被拒绝")
	}
	var re *RoleError
	if !asRoleError(err, &re) || !re.NeedUser {
		t.Fatalf("应返回 NeedUser 的 RoleError，得到 %#v", err)
	}
}

// TestEmptyExecutorRejected 空执行者必须被拒。
func TestEmptyExecutorRejected(t *testing.T) {
	if err := validateAssignment(contract.RoleAssignment{Executor: "  "}); err == nil {
		t.Fatal("空执行者应被拒绝")
	}
}

// TestHelpersDedupedAndCleaned helpers 去重且不与执行者/审查者重复。
func TestHelpersDedupedAndCleaned(t *testing.T) {
	next, _, err := resolveAssignment(nil, contract.RoleAssignment{
		Executor: "codex", Reviewer: "trae",
		Helpers: []string{"doubao", "doubao", "codex", "trae", " "},
		Source:  contract.SourceUserChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Helpers) != 1 || next.Helpers[0] != "doubao" {
		t.Fatalf("helpers 应只剩 doubao，得到 %v", next.Helpers)
	}
}

// ---- 聊天指令识别（§5.8 咨询 ≠ 改分工） ----

// TestConsultDoesNotChangeRoles @某人 + 描述 = 咨询，不改分工。
func TestConsultDoesNotChangeRoles(t *testing.T) {
	known := func(id string) bool { return id == "codex" || id == "trae" }
	in := interpretRoleInstruction("@trae 看看这个实现有没有问题", []string{"trae"}, known)
	if in.Kind != InstrConsult {
		t.Fatalf("期望 consult，得到 %s", in.Kind)
	}
	if !in.KeepExisting {
		t.Fatal("咨询必须保持原有分工")
	}
	if in.Executor != "" || in.Reviewer != "" {
		t.Fatalf("咨询不得产生角色变更，得到 %+v", in)
	}
}

// TestExplicitRoleChange 明确说"改由 B 审查"才是改分工。
func TestExplicitRoleChange(t *testing.T) {
	known := func(id string) bool { return id == "codex" || id == "trae" || id == "dsh" }
	in := interpretRoleInstruction("改由 dsh 审查", []string{"dsh"}, known)
	if in.Kind != InstrRoleChange {
		t.Fatalf("期望 role_change，得到 %s", in.Kind)
	}
	if in.Reviewer != "dsh" {
		t.Fatalf("期望 reviewer=dsh，得到 %q", in.Reviewer)
	}
}

// TestRoleChangeWithoutMentionFindsBot 不打 @ 直接写"改由 doubao 执行"也要认出来。
//
// 用户只有一个聊天框，让他非得打 @ 才能换人是多余的；但只认整词，
// 拼错或没这个人时宁可问，不猜。
func TestRoleChangeWithoutMentionFindsBot(t *testing.T) {
	fs := &fakeStore{seen: map[string]bool{}}
	o := newOrch(fs)
	w := baseWork(withReviewer())
	fs.work = &w

	if _, _, err := o.OnUserMessage(bg(), contract.Message{
		ID: "m7", WorkID: w.ID, Kind: contract.MsgChat,
		Text: "改由doubao执行", FromID: "user",
	}); err != nil {
		t.Fatalf("处理失败：%v", err)
	}
	if got := fs.work.Assign.Executor; got != "doubao" {
		t.Fatalf("不打 @ 也应认出 doubao，得到 %s", got)
	}
}

// TestBotsInTextNeedsWholeWord 前缀不算命中：codex 不该被 codex-mini 认成自己。
func TestBotsInTextNeedsWholeWord(t *testing.T) {
	fs := &fakeStore{seen: map[string]bool{}}
	o := newOrch(fs)

	if got := o.botsInText("让 codex-mini 试试"); len(got) != 0 {
		t.Fatalf("codex 不该命中 codex-mini，得到 %v", got)
	}
	if got := o.botsInText("让 codex 试试"); len(got) != 1 || got[0] != "codex" {
		t.Fatalf("应认出 codex，得到 %v", got)
	}
}

// TestUnknownBotInTextIsNotGuessed 文本里的人不在房间时，不许顶替成别人。
func TestUnknownBotInTextIsNotGuessed(t *testing.T) {
	fs := &fakeStore{seen: map[string]bool{}}
	o := newOrch(fs)
	w := baseWork(withReviewer())
	fs.work = &w
	before := w.Assign.Executor

	if _, _, err := o.OnUserMessage(bg(), contract.Message{
		ID: "m8", WorkID: w.ID, Kind: contract.MsgChat,
		Text: "改由 ghost 执行", FromID: "user",
	}); err != nil {
		t.Fatalf("处理失败：%v", err)
	}
	if got := fs.work.Assign.Executor; got != before {
		t.Fatalf("认不出来时不得换人，%s → %s", before, got)
	}
}

// TestAmbiguousMentionsAskOnlyThatPart 不存在的 @ 只就那部分提问，不换人。
func TestAmbiguousMentionsAskOnlyThatPart(t *testing.T) {
	known := func(id string) bool { return id == "codex" }
	in := interpretRoleInstruction("@ghost 你看下", []string{"ghost"}, known)
	if len(in.Ambiguous) != 1 || in.Ambiguous[0] != "ghost" {
		t.Fatalf("应把 ghost 列为待澄清，得到 %v", in.Ambiguous)
	}
	if in.Executor != "" {
		t.Fatal("不得自行替换执行者")
	}
}

// TestMultiTargetRoleChangeAmbiguous 一句话点多人又没说清 → 问，不猜。
func TestMultiTargetRoleChangeAmbiguous(t *testing.T) {
	known := func(id string) bool { return id == "codex" || id == "trae" }
	in := interpretRoleInstruction("改由 codex 和 trae 审查", []string{"codex", "trae"}, known)
	if len(in.Ambiguous) != 2 {
		t.Fatalf("应列为待澄清，得到 %v", in.Ambiguous)
	}
	if in.Reviewer != "" {
		t.Fatal("不确定时不得指定审查者")
	}
	if !in.KeepExisting {
		t.Fatal("不确定时必须保持原有分工")
	}
}

// TestSkipReviewPhrase 明确不要审查。
func TestSkipReviewPhrase(t *testing.T) {
	in := interpretRoleInstruction("不用审查，直接给我", nil, nil)
	if in.Kind != InstrSkipReview || !in.ClearReviewer {
		t.Fatalf("期望 skip_review，得到 %+v", in)
	}
}

// TestPlainChatNoInstruction 普通聊天不产生任何分工动作。
func TestPlainChatNoInstruction(t *testing.T) {
	in := interpretRoleInstruction("今天先做登录页", nil, nil)
	if in.Kind != InstrNone {
		t.Fatalf("期望 none，得到 %s", in.Kind)
	}
}

// ---- 审查参数解析（解析失败绝不 approved） ----

// TestReviewParseFailureNeverApproves 各种坏参数都不得得到 approved。
func TestReviewParseFailureNeverApproves(t *testing.T) {
	bad := []string{
		``,
		`not json`,
		`{"verdict":"lgtm","findings":"ok"}`,
		`{"verdict":"","findings":"ok"}`,
		`{"verdict":"approved"}`, // 缺 artifact_version
		`{"verdict":"approved","artifact_version":1}`,                            // 缺 findings
		`{"verdict":"approved","artifact_version":1,"findings":"x","version":1}`, // 未知字段
		`{"verdict":"changes_requested"}`,                                        // 缺 findings
	}
	for _, raw := range bad {
		got, err := parseReviewArgs(raw)
		if err == nil {
			t.Fatalf("参数 %q 应报错，但通过了：%+v", raw, got)
		}
		if got.Verdict == string(contract.VerdictApproved) {
			t.Fatalf("参数 %q 不得解析为 approved", raw)
		}
	}
}

// TestReviewParseApprovedOK 合法的 approved 能通过。
func TestReviewParseApprovedOK(t *testing.T) {
	a, err := parseReviewArgs(`{"verdict":"APPROVED","artifact_version":2,"findings":"验过了"}`)
	if err != nil {
		t.Fatal(err)
	}
	if a.Verdict != string(contract.VerdictApproved) || a.ArtifactVersion != 2 {
		t.Fatalf("解析结果不对：%+v", a)
	}
}

// TestSubmitArgsRejectsUnlocatableArtifact 产物必须能定位。
func TestSubmitArgsRejectsUnlocatableArtifact(t *testing.T) {
	if _, err := parseSubmitArgs(`{"summary":"做完了","kind":"file"}`); err == nil {
		t.Fatal("既无 ref 又无 files 应被拒绝")
	}
	if _, err := parseSubmitArgs(`{"kind":"file","ref":"a.md"}`); err == nil {
		t.Fatal("缺 summary 应被拒绝")
	}
	if _, err := parseSubmitArgs(`{"summary":"s","kind":"file","ref":"a.md"}`); err != nil {
		t.Fatalf("合法参数应通过，得到 %v", err)
	}
}

// ---- 护栏 ----

// TestGuardDepthLimit 深度超限被拦，且带用户可见 notice。
func TestGuardDepthLimit(t *testing.T) {
	g := newGuardState()
	w := baseWork(withReviewer())
	v := g.Check(bg(), &fakeStore{}, &w, "m1", WakeTarget{
		BotID: "codex", Depth: MaxHandoffDepth + 1})
	if v.Allow {
		t.Fatal("深度超限应被拦")
	}
	if v.Reason != GuardDepth {
		t.Fatalf("期望 depth，得到 %s", v.Reason)
	}
	if v.Notice == "" {
		t.Fatal("超限必须给出用户可见提示，不能静默")
	}
}

// TestGuardWakeupLimit 唤醒总次数超限被拦。
func TestGuardWakeupLimit(t *testing.T) {
	g := newGuardState()
	w := baseWork(withReviewer())
	var last GuardVerdict
	for i := 0; i <= MaxWakeupsPerWork+1; i++ {
		last = g.Check(bg(), &fakeStore{}, &w, "m"+itoa(i), WakeTarget{BotID: "codex"})
	}
	if last.Allow || last.Reason != GuardWakeups {
		t.Fatalf("唤醒超限应被拦，得到 %+v", last)
	}
	if last.Notice == "" {
		t.Fatal("超限必须给出用户可见提示")
	}
}

// TestGuardDuplicate 同一 (message, bot) 第二次被去重，且不需要打扰用户。
func TestGuardDuplicate(t *testing.T) {
	g := newGuardState()
	w := baseWork(withReviewer())
	fs := &fakeStore{seen: map[string]bool{}}
	t1 := WakeTarget{BotID: "codex"}
	if v := g.Check(bg(), fs, &w, "m1", t1); !v.Allow {
		t.Fatalf("首次应放行，得到 %+v", v)
	}
	v := g.Check(bg(), fs, &w, "m1", t1)
	if v.Allow || v.Reason != GuardDuplicate {
		t.Fatalf("重复应被去重，得到 %+v", v)
	}
	if v.Notice != "" {
		t.Fatal("去重命中不该打扰用户（那条消息已经送到过）")
	}
}

// TestGuardCooldownMutes 短时间内反复唤醒触发静音。
func TestGuardCooldownMutes(t *testing.T) {
	g := newGuardState()
	w := baseWork(withReviewer())
	fs := &fakeStore{seen: map[string]bool{}}
	var last GuardVerdict
	for i := 0; i <= CooldownCount+1; i++ {
		last = g.Check(bg(), fs, &w, "m"+itoa(i), WakeTarget{BotID: "codex"})
	}
	if last.Allow || last.Reason != GuardCooldown {
		t.Fatalf("应触发冷却，得到 %+v", last)
	}
	if last.Notice == "" {
		t.Fatal("静音必须告知用户")
	}
	if len(g.MutedBots()) == 0 {
		t.Fatal("应记录静音状态")
	}
	g.Unmute("codex")
	if len(g.MutedBots()) != 0 {
		t.Fatal("解除静音后应为空")
	}
}

// TestGuardBudgetUnknownUsageHonest 配了 token 上限但客户端不上报：
// 必须如实告知"卡不住"，而不是假装拦住。
func TestGuardBudgetUnknownUsageHonest(t *testing.T) {
	g := newGuardState()
	w := baseWork(withReviewer())
	w.Budget = &contract.Budget{DailyTokens: 1000}
	fs := &fakeStore{seen: map[string]bool{}}
	g.RecordUsage(w.ID, contract.Usage{Known: false}, 0)

	v := g.Check(bg(), fs, &w, "m1", WakeTarget{BotID: "codex"})
	if v.Allow {
		t.Fatal("应触发一次诚实提示")
	}
	if v.Reason != GuardBudget || v.Notice == "" {
		t.Fatalf("应说明无法硬卡 token，得到 %+v", v)
	}
	// 只提示一次，不反复骚扰
	if v2 := g.Check(bg(), fs, &w, "m2", WakeTarget{BotID: "codex"}); !v2.Allow {
		t.Fatalf("第二次不应再因同一原因拦截，得到 %+v", v2)
	}
}

// TestGuardBudgetKnownUsageBlocks 已知用量超上限时真的拦。
func TestGuardBudgetKnownUsageBlocks(t *testing.T) {
	g := newGuardState()
	w := baseWork(withReviewer())
	w.Budget = &contract.Budget{DailyTokens: 100}
	fs := &fakeStore{seen: map[string]bool{}}
	g.RecordUsage(w.ID, contract.Usage{Known: true, TokensIn: 80, TokensOut: 40}, 0)

	v := g.Check(bg(), fs, &w, "m1", WakeTarget{BotID: "codex"})
	if v.Allow || v.Reason != GuardBudget {
		t.Fatalf("已知用量超限应拦住，得到 %+v", v)
	}
}

// ---- 外部副作用五态（§5.8） ----

// TestEffectExecutingStopIsUnconfirmed executing 的动作停止只能是 unknown。
func TestEffectExecutingStopIsUnconfirmed(t *testing.T) {
	l := newEffectLog()
	k := EffectKey{WorkID: "w1", Step: "send"}
	l.Begin(k, at(0))
	l.Executing(k, at(1))

	state, notice := l.RequestStop(k, at(2))
	if state != contract.OpUnknown {
		t.Fatalf("executing 的动作停止后必须是 unknown，得到 %s", state)
	}
	if notice != StopPendingNotice {
		t.Fatalf("文案必须是停止待确认，得到 %q", notice)
	}
}

// TestEffectPreparedStopIsDefinite 还没发出去的动作可以确定地取消。
func TestEffectPreparedStopIsDefinite(t *testing.T) {
	l := newEffectLog()
	k := EffectKey{WorkID: "w1", Step: "send"}
	l.Begin(k, at(0))

	state, notice := l.RequestStop(k, at(1))
	if state != contract.OpFailed {
		t.Fatalf("未发出的动作取消后应为 failed，得到 %s", state)
	}
	if notice != StopDoneNotice {
		t.Fatalf("文案应为已停止，得到 %q", notice)
	}
}

// TestEffectFinishedStopIsNoop 已经结束的动作停止没有作用，也不改状态。
func TestEffectFinishedStopIsNoop(t *testing.T) {
	l := newEffectLog()
	k := EffectKey{WorkID: "w1", Step: "send"}
	l.Begin(k, at(0))
	l.Succeed(k, at(1))

	state, _ := l.RequestStop(k, at(2))
	if state != contract.OpSucceeded {
		t.Fatalf("已成功的动作不应被停止改成别的状态，得到 %s", state)
	}
}

// TestEffectNeverRegisteredStopIsUnknown 从未登记的动作停止也必须是 unknown，不许猜成功。
func TestEffectNeverRegisteredStopIsUnknown(t *testing.T) {
	l := newEffectLog()
	state, notice := l.RequestStop(EffectKey{WorkID: "w1", Step: "nope"}, at(0))
	if state != contract.OpUnknown || notice != StopPendingNotice {
		t.Fatalf("未登记的动作停止应为 unknown + 停止待确认，得到 %s / %q", state, notice)
	}
}

// TestEffectUnknownStaysPending unknown 必须留在待交代列表里。
func TestEffectUnknownStaysPending(t *testing.T) {
	l := newEffectLog()
	k := EffectKey{WorkID: "w1", Step: "send"}
	l.Lose(k, "窗口没了", at(0))

	p := l.Pending()
	if len(p) != 1 || p[0].State != contract.OpUnknown {
		t.Fatalf("unknown 必须留在 Pending 里，得到 %+v", p)
	}
	if got := l.ForWork("w1"); len(got) != 1 {
		t.Fatalf("ForWork 应列出该 Work 的动作，得到 %+v", got)
	}
}

// ---- 停止一个 Work ----

// newOrch 造一个能跑指令的编排层（engine 传 nil：这些路径不该碰引擎）。
func newOrch(fs *fakeStore) *Orchestrator {
	return New(fs, nil, []contract.BotSpec{
		{ID: "codex", Name: "Codex"},
		{ID: "trae", Name: "Trae"},
		{ID: "doubao", Name: "豆包"},
	})
}

// TestCancelReportsStopPending 有在途动作时停止必须说"停止待确认"。
func TestCancelReportsStopPending(t *testing.T) {
	fs := &fakeStore{seen: map[string]bool{}}
	o := newOrch(fs)
	w := baseWork(withReviewer())
	fs.work = &w

	o.BeginEffect(w.ID, "send")
	o.EffectExecuting(w.ID, "send")

	if err := o.cancel(bg(), &w); err != nil {
		t.Fatalf("停止失败：%v", err)
	}
	if w.State != contract.WorkCancelled {
		t.Fatalf("状态应为 cancelled，得到 %s", w.State)
	}
	if got := o.cancelNotice(&w); got != StopPendingNotice {
		t.Fatalf("有在途动作时必须报停止待确认，得到 %q", got)
	}
}

// TestCancelWithNoEffectsSaysStopped 没有在途动作时才能直接说已停止。
func TestCancelWithNoEffectsSaysStopped(t *testing.T) {
	fs := &fakeStore{seen: map[string]bool{}}
	o := newOrch(fs)
	w := baseWork(withReviewer())
	fs.work = &w

	if err := o.cancel(bg(), &w); err != nil {
		t.Fatalf("停止失败：%v", err)
	}
	if got := o.cancelNotice(&w); got != "已停止这个委托。" {
		t.Fatalf("无在途动作时应直接说已停止，得到 %q", got)
	}
}

// TestCancelCountsMultipleUnconfirmed 多个在途动作时要说清有几个。
func TestCancelCountsMultipleUnconfirmed(t *testing.T) {
	fs := &fakeStore{seen: map[string]bool{}}
	o := newOrch(fs)
	w := baseWork(withReviewer())
	fs.work = &w

	for _, step := range []string{"a", "b"} {
		o.BeginEffect(w.ID, step)
		o.EffectExecuting(w.ID, step)
	}
	if err := o.cancel(bg(), &w); err != nil {
		t.Fatalf("停止失败：%v", err)
	}
	got := o.cancelNotice(&w)
	if !strings.Contains(got, "2 个") {
		t.Fatalf("应报出 2 个未确认动作，得到 %q", got)
	}
}

// ---- 暂停 / 继续 / 停止（聊天控制） ----

// TestPauseVerbPauses 用户说「暂停」应真的暂停，并且不再唤醒任何人。
func TestPauseVerbPauses(t *testing.T) {
	fs := &fakeStore{seen: map[string]bool{}}
	o := newOrch(fs)
	w := baseWork(withReviewer())
	fs.work = &w

	targets, notice, err := o.OnUserMessage(bg(), contract.Message{
		ID: "m1", WorkID: w.ID, Kind: contract.MsgChat, Text: "暂停", FromID: "user",
	})
	if err != nil {
		t.Fatalf("处理失败：%v", err)
	}
	if got := fs.work.State; got != contract.WorkPaused {
		t.Fatalf("应暂停，得到 %s", got)
	}
	if notice == "" {
		t.Fatal("暂停必须给用户一句话")
	}
	if len(targets) != 0 {
		t.Fatalf("暂停后不应唤醒任何人，得到 %+v", targets)
	}
}

// TestResumeVerbResumes 「继续」从暂停恢复到 queued。
func TestResumeVerbResumes(t *testing.T) {
	fs := &fakeStore{seen: map[string]bool{}}
	o := newOrch(fs)
	w := baseWork(withReviewer())
	fs.work = &w
	if err := o.pause(bg(), &w); err != nil {
		t.Fatalf("暂停失败：%v", err)
	}

	if _, _, err := o.OnUserMessage(bg(), contract.Message{
		ID: "m2", WorkID: w.ID, Kind: contract.MsgChat, Text: "继续", FromID: "user",
	}); err != nil {
		t.Fatalf("处理失败：%v", err)
	}
	if got := fs.work.State; got != contract.WorkQueued {
		t.Fatalf("应恢复到 queued，得到 %s", got)
	}
}

// TestStopVerbCancels 「停止」应取消并如实交代在途动作。
func TestStopVerbCancels(t *testing.T) {
	fs := &fakeStore{seen: map[string]bool{}}
	o := newOrch(fs)
	w := baseWork(withReviewer())
	fs.work = &w
	o.BeginEffect(w.ID, "send")
	o.EffectExecuting(w.ID, "send")

	_, notice, err := o.OnUserMessage(bg(), contract.Message{
		ID: "m3", WorkID: w.ID, Kind: contract.MsgChat, Text: "停止", FromID: "user",
	})
	if err != nil {
		t.Fatalf("处理失败：%v", err)
	}
	if got := fs.work.State; got != contract.WorkCancelled {
		t.Fatalf("应取消，得到 %s", got)
	}
	if notice != StopPendingNotice {
		t.Fatalf("有在途动作时必须报停止待确认，得到 %q", notice)
	}
}

// TestChatWordContinueIsNotControl 普通聊天里出现「继续」不该被当成控制指令。
func TestChatWordContinueIsNotControl(t *testing.T) {
	fs := &fakeStore{seen: map[string]bool{}}
	o := newOrch(fs)
	w := baseWork(withReviewer())
	fs.work = &w
	before := w.State

	if _, _, err := o.OnUserMessage(bg(), contract.Message{
		ID: "m4", WorkID: w.ID, Kind: contract.MsgChat,
		Text: "继续把第三节补一下，顺便看看这段", FromID: "user",
	}); err != nil {
		t.Fatalf("处理失败：%v", err)
	}
	if got := fs.work.State; got != before {
		t.Fatalf("普通聊天不该改状态，%s → %s", before, got)
	}
}

// TestUserChatRoleChangeBeatsEverything 用户在聊天里换人，版本号加一且来源是 user_chat。
func TestUserChatRoleChangeBeatsEverything(t *testing.T) {
	fs := &fakeStore{seen: map[string]bool{}}
	o := newOrch(fs)
	w := baseWork(withReviewer())
	fs.work = &w

	_, notice, err := o.OnUserMessage(bg(), contract.Message{
		ID: "m5", WorkID: w.ID, Kind: contract.MsgChat,
		Text: "改由 doubao 执行", FromID: "user",
	})
	if err != nil {
		t.Fatalf("处理失败：%v", err)
	}
	if got := fs.work.Assign.Executor; got != "doubao" {
		t.Fatalf("执行者应换成 doubao，得到 %s", got)
	}
	if got := fs.work.Assign.Source; got != contract.SourceUserChat {
		t.Fatalf("来源应为 user_chat，得到 %s", got)
	}
	if got := fs.work.Assign.Version; got != 2 {
		t.Fatalf("版本号应加一到 2，得到 %d", got)
	}
	if notice == "" {
		t.Fatal("换人必须告诉用户现在的分工")
	}
}

// TestConsultWakesWithoutChangingRoles 咨询只唤醒被 @ 的人，分工不变。
func TestConsultWakesWithoutChangingRoles(t *testing.T) {
	fs := &fakeStore{seen: map[string]bool{}}
	o := newOrch(fs)
	w := baseWork(withReviewer())
	fs.work = &w
	before := w.Assign

	targets, _, err := o.OnUserMessage(bg(), contract.Message{
		ID: "m6", WorkID: w.ID, Kind: contract.MsgChat,
		Text: "@doubao 看看这个思路", Mentions: []string{"doubao"}, FromID: "user",
	})
	if err != nil {
		t.Fatalf("处理失败：%v", err)
	}
	if got := fs.work.Assign; got.Executor != before.Executor || got.Reviewer != before.Reviewer {
		t.Fatalf("咨询不该改分工：%+v → %+v", before, got)
	}
	found := false
	for _, tg := range targets {
		if tg.BotID == "doubao" && tg.Reason == WakeMention {
			found = true
		}
	}
	if !found {
		t.Fatalf("被 @ 的人应以 mention 唤醒，得到 %+v", targets)
	}
}

// ---- 并发名额（§5.6） ----

// TestSchedulerPerBotSerial 同一个 bot 同时只能有一个 Run。
func TestSchedulerPerBotSerial(t *testing.T) {
	s := newScheduler()
	if _, err := s.TryStart("w1", "codex", "r1"); err != nil {
		t.Fatalf("第一个 Run 应能启动：%v", err)
	}
	if _, err := s.TryStart("w2", "codex", "r2"); !errors.Is(err, ErrBotBusy) {
		t.Fatalf("同 bot 第二个 Run 应返回 ErrBotBusy，得到 %v", err)
	}
}

// TestSchedulerGlobalCap 全局最多 MaxActiveRuns 个活跃 Run。
func TestSchedulerGlobalCap(t *testing.T) {
	s := newScheduler()
	for i := 0; i < MaxActiveRuns; i++ {
		if _, err := s.TryStart("w"+itoa(i), "bot"+itoa(i), "r"+itoa(i)); err != nil {
			t.Fatalf("第 %d 个 Run 应能启动：%v", i, err)
		}
	}
	if _, err := s.TryStart("wx", "botx", "rx"); !errors.Is(err, ErrSchedulerFull) {
		t.Fatalf("超出上限应返回 ErrSchedulerFull，得到 %v", err)
	}
}

// TestSchedulerFairAcrossWorks 别的 Work 排一个就能插到同一个 Work 的第二个之前。
func TestSchedulerFairAcrossWorks(t *testing.T) {
	s := newScheduler()
	s.Enqueue("w1", "codex", "r1")
	s.Enqueue("w1", "trae", "r2")
	s.Enqueue("w2", "doubao", "r3")

	first := s.Next()
	if first == nil {
		t.Fatal("应能取到第一个 Run")
	}
	if first.WorkID != "w1" {
		t.Fatalf("第一个应是 w1 的，得到 %+v", first)
	}
	second := s.Next()
	if second == nil || second.WorkID != "w2" {
		t.Fatalf("第二个名额应给 w2（不许 w1 连占），得到 %+v", second)
	}
	s.Release(first)
	s.Release(second)
	third := s.Next()
	if third == nil || third.WorkID != "w1" || third.BotID != "trae" {
		t.Fatalf("第三个才轮到 w1 的第二个，得到 %+v", third)
	}
}

// TestSchedulerReleaseIdempotent 重复归还名额不能把计数还成负数。
func TestSchedulerReleaseIdempotent(t *testing.T) {
	s := newScheduler()
	tk, err := s.TryStart("w1", "codex", "r1")
	if err != nil {
		t.Fatalf("应能启动：%v", err)
	}
	s.Release(tk)
	s.Release(tk)
	if got := s.Active(); got != 0 {
		t.Fatalf("归还两次后活跃数应为 0，得到 %d", got)
	}
	s.Release(nil) // nil 也不能炸
}

// TestSchedulerForgetWorkClearsQueue Work 终结后队列要清干净。
func TestSchedulerForgetWorkClearsQueue(t *testing.T) {
	s := newScheduler()
	s.Enqueue("w1", "codex", "r1")
	s.Enqueue("w2", "trae", "r2")
	s.ForgetWork("w1")
	if got := s.Queued(); got != 1 {
		t.Fatalf("清掉 w1 后应只剩 1 个排队，得到 %d", got)
	}
	tk := s.Next()
	if tk == nil || tk.WorkID != "w2" {
		t.Fatalf("应取到 w2，得到 %+v", tk)
	}
}

// TestStartRunQueuesWhenFull 名额满时 StartRun 自动排队，不报故障。
func TestStartRunQueuesWhenFull(t *testing.T) {
	o := newOrch(&fakeStore{seen: map[string]bool{}})
	for i := 0; i < MaxActiveRuns; i++ {
		if _, err := o.StartRun("w"+itoa(i), "bot"+itoa(i), "r"+itoa(i)); err != nil {
			t.Fatalf("第 %d 个 Run 应能启动：%v", i, err)
		}
	}
	if _, err := o.StartRun("wx", "botx", "rx"); !errors.Is(err, ErrSchedulerFull) {
		t.Fatalf("应返回 ErrSchedulerFull，得到 %v", err)
	}
	if got := o.QueuedRuns(); got != 1 {
		t.Fatalf("应自动入队 1 个，得到 %d", got)
	}
}

// TestWaitExternalFreesSlot 等外部时必须归还名额，否则会死锁（§5.6）。
func TestWaitExternalFreesSlot(t *testing.T) {
	o := newOrch(&fakeStore{seen: map[string]bool{}})
	tk, err := o.StartRun("w1", "codex", "r1")
	if err != nil {
		t.Fatalf("应能启动：%v", err)
	}
	o.WaitExternal(tk)
	if got := o.ActiveRuns(); got != 0 {
		t.Fatalf("等外部时应归还名额，活跃数应为 0，得到 %d", got)
	}
	// 名额还回来之后别人能跑
	if _, err := o.StartRun("w2", "trae", "r2"); err != nil {
		t.Fatalf("名额归还后应能启动新的 Run：%v", err)
	}
	o.FinishRun(tk) // 之后照常收尾，幂等
	if got := o.ActiveRuns(); got != 1 {
		t.Fatalf("收尾不应影响另一个 Run，得到 %d", got)
	}
}

// ---- 测试替身 ----

// fakeStore 只实现护栏与指令用得到的方法；其余方法返回 ErrNotFound。
type fakeStore struct {
	seen    map[string]bool
	work    *contract.Work
	saved   []contract.Work
	notices []contract.Message
}

func (f *fakeStore) MarkInputSeen(_ context.Context, messageID, botID string) (bool, error) {
	k := messageID + "|" + botID
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[k] {
		return true, nil
	}
	f.seen[k] = true
	return false, nil
}

func (f *fakeStore) SaveWork(_ context.Context, w contract.Work) error {
	f.saved = append(f.saved, w)
	if f.work != nil {
		cp := w
		f.work = &cp
	}
	return nil
}
func (f *fakeStore) GetWork(context.Context, string) (contract.Work, error) {
	if f.work == nil {
		return contract.Work{}, ErrNotFound
	}
	return *f.work, nil
}
func (f *fakeStore) ListWorks(context.Context, string, []contract.WorkState) ([]contract.Work, error) {
	return nil, nil
}
func (f *fakeStore) SaveMessage(_ context.Context, m contract.Message) error {
	f.notices = append(f.notices, m)
	return nil
}
func (f *fakeStore) GetMessage(context.Context, string) (contract.Message, error) {
	return contract.Message{}, ErrNotFound
}
func (f *fakeStore) SaveReview(context.Context, contract.Review) error          { return nil }
func (f *fakeStore) Reviews(context.Context, string) ([]contract.Review, error) { return nil, nil }
func (f *fakeStore) SaveAssignment(context.Context, string, contract.RoleAssignment) error {
	return nil
}
func (f *fakeStore) Assignments(context.Context, string) ([]contract.RoleAssignment, error) {
	return nil, nil
}
func (f *fakeStore) AppendEvent(context.Context, contract.Event) error     { return nil }
func (f *fakeStore) PendingFor(context.Context, string) ([]Pending, error) { return nil, nil }
func (f *fakeStore) RevokePending(context.Context, string, string) error   { return nil }
