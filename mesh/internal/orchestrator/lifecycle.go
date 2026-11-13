package orchestrator

// Work 生命周期推进的导出接口（P0 派发器使用）。
//
// 派发器（app 层）领取投递后需要把 Work 从 queued 推进到 executing，
// 引擎产出一轮完整结果后需要完成委托。这些状态迁移属于编排层的职责
// （状态机合法性校验在这里），派发器只调方法、不直接改 Work 字段落库。

import (
	"context"
	"errors"

	"mesh/internal/contract"
)

// ErrWorkState 表示迁移与状态机不符（调用方时机错了，不该静默吞）。
var ErrWorkState = errors.New("orchestrator: Work 状态迁移不合法")

// MarkExecuting 把 queued 的 Work 推进到 executing。幂等：
// 已经是 executing 或更后阶段时原样返回 nil。
func (o *Orchestrator) MarkExecuting(ctx context.Context, workID string) error {
	w, err := o.store.GetWork(ctx, workID)
	if err != nil {
		return err
	}
	if w.State != contract.WorkQueued {
		return nil
	}
	if err := transition(&w, contract.WorkExecuting, o.now()); err != nil {
		return err
	}
	return o.store.SaveWork(ctx, w)
}

// CompleteWork 在引擎产出一轮结果后完成委托（executing → completed）。
//
// 只有"无审查者"的委托允许由派发层直接完成（§5.7：无审查要求时
// executing → completed）；有审查者的委托必须走 review.submit 的审查流程，
// 这里如实拒绝并返回 ErrWorkState——派发层不该代替审查者放行。
func (o *Orchestrator) CompleteWork(ctx context.Context, workID string) error {
	w, err := o.store.GetWork(ctx, workID)
	if err != nil {
		return err
	}
	if w.State != contract.WorkExecuting {
		return nil
	}
	if w.Assign.Reviewer != "" {
		return ErrWorkState
	}
	if err := transition(&w, contract.WorkCompleted, o.now()); err != nil {
		return err
	}
	return o.store.SaveWork(ctx, w)
}
