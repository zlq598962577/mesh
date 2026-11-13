package orchestrator

import (
	"fmt"
	"strings"
	"time"

	"mesh/internal/contract"
)

// workStateTransitions 合法的状态迁移（§5.7 状态机）。
//
// queued → executing → reviewing → completed
// reviewing → executing（要求修改）
// 无审查者：executing → completed
// 自动修改最多 MaxAutoReviewRounds 轮，仍不通过 → waiting_user
// paused / waiting_user / cancelled / failed 是中断态；failed 可重试回 queued。
//
// 入口（queued）与出口（completed/failed/cancelled）由 orchestrate() 统一处理，
// 这里只校验单步迁移是否合法，非法迁移直接返回错误，绝不静默吞掉。
var workStateTransitions = map[contract.WorkState]map[contract.WorkState]bool{
	contract.WorkQueued: {
		contract.WorkExecuting:   true, // 开始执行
		contract.WorkPaused:      true, // 排队中被暂停
		contract.WorkCancelled:   true, // 排队中被取消
		contract.WorkWaitingUser: true, // 排队中需要用户澄清（如分工缺失）
	},
	contract.WorkExecuting: {
		contract.WorkReviewing:   true, // 执行者提交产物，进入审查
		contract.WorkCompleted:   true, // 无审查者，直接完成
		contract.WorkPaused:      true,
		contract.WorkWaitingUser: true, // 审查轮次超限，交回用户
		contract.WorkFailed:      true, // 执行失败
		contract.WorkCancelled:   true,
	},
	contract.WorkReviewing: {
		contract.WorkExecuting:   true, // 要求修改，回到执行者
		contract.WorkCompleted:   true, // 审查通过（绑定产物版本）
		contract.WorkWaitingUser: true, // 自动修改轮次超限
		contract.WorkPaused:      true,
		contract.WorkCancelled:   true,
	},
	contract.WorkPaused: {
		contract.WorkQueued:      true, // 恢复排队
		contract.WorkExecuting:   true, // 恢复执行
		contract.WorkCancelled:   true,
		contract.WorkWaitingUser: true,
	},
	contract.WorkWaitingUser: {
		contract.WorkQueued:    true, // 用户澄清后重新排队
		contract.WorkExecuting: true, // 用户澄清后直接执行
		contract.WorkCancelled: true,
		contract.WorkCompleted: true, // 用户明确接受当前产物
	},
	contract.WorkFailed: {
		contract.WorkQueued:    true, // 用户要求重试
		contract.WorkCancelled: true,
	},
	contract.WorkCompleted: {}, // 终态
	contract.WorkCancelled: {}, // 终态
}

// WorkError 状态机拒绝某次迁移时返回的错误，携带原因供 UI 展示。
type WorkError struct {
	WorkID string
	From   contract.WorkState
	To     contract.WorkState
	Reason string
}

func (e *WorkError) Error() string {
	return fmt.Sprintf("work %s: 不允许从 %s 迁移到 %s：%s", e.WorkID, e.From, e.To, e.Reason)
}

// newWork 创建一个排队中的委托（§5.7 起点 queued）。
// assign 由 roles.go 的 resolveAssignment 在创建前填好；调用方保证非空 executor。
func newWork(id, roomID, threadID, goal string, assign contract.RoleAssignment, budget *contract.Budget, now time.Time) contract.Work {
	return contract.Work{
		ID:           id,
		RoomID:       roomID,
		ThreadID:     threadID,
		Goal:         goal,
		State:        contract.WorkQueued,
		Assign:       assign,
		ReviewRounds: 0,
		Budget:       budget,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

// canTransition 判断 from→to 是否在 §5.7 状态机内。
func canTransition(from, to contract.WorkState) bool {
	return workStateTransitions[from][to]
}

// transition 执行迁移并刷新 UpdatedAt。非法迁移返回 *WorkError。
func transition(w *contract.Work, to contract.WorkState, now time.Time) error {
	if w.State == to {
		return nil // 幂等：同一状态不重复记
	}
	if !canTransition(w.State, to) {
		return &WorkError{WorkID: w.ID, From: w.State, To: to,
			Reason: "不在 §5.7 允许的迁移表内"}
	}
	w.State = to
	w.UpdatedAt = now
	return nil
}

// requiresReview 分工是否配置了审查者。
func requiresReview(assign contract.RoleAssignment) bool {
	return strings.TrimSpace(assign.Reviewer) != ""
}

// onArtifactSubmitted 执行者提交产物时的状态推进（§5.5 work.submit）。
//
//   - 无审查者：executing → completed，产物即最终产物。
//   - 有审查者：executing → reviewing，等审查结论。
//
// version 必须是该 Work 下一个未用过的版本号（调用方从 Artifacts 长度推导）。
// 返回写好的 Review 草稿（verdict 尚未填，由 onReviewVerdict 处理）。
func onArtifactSubmitted(w *contract.Work, art contract.ArtifactRef, now time.Time) error {
	if w.State != contract.WorkExecuting {
		return &WorkError{WorkID: w.ID, From: w.State, To: contract.WorkReviewing,
			Reason: "提交产物只允许在执行中"}
	}
	if requiresReview(w.Assign) {
		return transition(w, contract.WorkReviewing, now)
	}
	// 无审查者：direct complete（§5.7 "无审查者时 executing → completed"）
	w.Final = &art
	return transition(w, contract.WorkCompleted, now)
}

// onReviewVerdict 审查者给出结论时的状态推进（§5.5 review.submit）。
//
// approved 只对 review.ArtifactVersion 指向的那个版本生效：
//   - 该版本正是当前最新产物版本 → completed；
//   - 该版本是旧版本（期间执行者又提交了新版本）→ 旧 approval 不得放行新版本，
//     仅记录，状态不迁移，必须等新版本的审查（§11 风险「旧版本获批」）。
//
// changes_requested → reviewing → executing，ReviewRounds+1；
//
//	若 ReviewRounds 已达 MaxAutoReviewRounds → waiting_user（自动修改封顶，§5.7）。
//
// blocked → waiting_user。
func onReviewVerdict(w *contract.Work, r contract.Review, now time.Time) error {
	switch r.Verdict {
	case contract.VerdictApproved:
		if w.State != contract.WorkReviewing {
			// 已在别处完成/中断，旧审批只是历史记录
			return nil
		}
		latest := latestVersion(w)
		if r.ArtifactVersion != latest {
			// 旧版本获批，新版本已存在：不放行（§11）。
			return &WorkError{WorkID: w.ID, From: w.State, To: contract.WorkCompleted,
				Reason: fmt.Sprintf("approved 绑定版本 %d，但最新版本是 %d，旧审批不得放行新版本", r.ArtifactVersion, latest)}
		}
		return onApprovedForLatest(w, r, now)

	case contract.VerdictChangesRequested:
		if w.State != contract.WorkReviewing {
			return &WorkError{WorkID: w.ID, From: w.State, To: contract.WorkExecuting,
				Reason: "要求修改只允许在审查中"}
		}
		w.ReviewRounds++
		if w.ReviewRounds > contract.MaxAutoReviewRounds {
			return transition(w, contract.WorkWaitingUser, now)
		}
		return transition(w, contract.WorkExecuting, now)

	case contract.VerdictBlocked:
		if w.State != contract.WorkReviewing {
			return &WorkError{WorkID: w.ID, From: w.State, To: contract.WorkWaitingUser,
				Reason: "blocked 只允许在审查中"}
		}
		return transition(w, contract.WorkWaitingUser, now)

	default:
		// 解析失败/未知结论：绝不自动判 approved（§5.5）。交回用户判断。
		return transition(w, contract.WorkWaitingUser, now)
	}
}

// onApprovedForLatest 审查者对该 Work 最新版本给出 approved 时的完成逻辑。
// 产物版本、审查结论、Work 状态三者必须指向同一个版本才算"通过"。
func onApprovedForLatest(w *contract.Work, r contract.Review, now time.Time) error {
	arts := w.Artifacts
	if r.ArtifactVersion < 1 || r.ArtifactVersion > len(arts) {
		return &WorkError{WorkID: w.ID, From: w.State, To: contract.WorkCompleted,
			Reason: fmt.Sprintf("approved 指向版本 %d，但产物只有 %d 个版本", r.ArtifactVersion, len(arts))}
	}
	// 完成判定绑定版本：Final 必须是这个被批准的版本，不能是"最新"（版本恰好相同才算）。
	// 若期间执行者又提交了更新的版本（len 更大），上面 latestVersion 已拦住。
	w.Final = &arts[r.ArtifactVersion-1]
	return transition(w, contract.WorkCompleted, now)
}

// latestVersion 当前产物最高版本号；无产物返回 0。
func latestVersion(w *contract.Work) int {
	return len(w.Artifacts)
}

// maxAutoRoundsExceeded 是否已超过自动修改轮次上限（供 orchestrate 决定是否转 waiting_user）。
func maxAutoRoundsExceeded(w *contract.Work) bool {
	return w.ReviewRounds > contract.MaxAutoReviewRounds
}

// touch 刷新 UpdatedAt。
func touch(w *contract.Work, now time.Time) {
	w.UpdatedAt = now
}
