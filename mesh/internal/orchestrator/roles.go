package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"mesh/internal/contract"
)

// assignmentRank 分工来源优先级（§12，数字越大越优先）。
//
//	用户对当前委托的最新明确指定（user_chat） > 发起时的界面选择（user_ui）
//	> 房间默认（room_default） > agent 的 soul 建议（soul_suggest，仅建议）
//
// soul 只作为最低优先级的建议：没有任何"让 agent 自己商量谁干活"的通路，
// 早期"人格驱动自组织"的方案已被否决，这里只保留它在优先级表的最底格。
var assignmentRank = map[contract.RoleSource]int{
	contract.SourceSoulSuggest: 1,
	contract.SourceRoomDefault: 2,
	contract.SourceUserUI:      3,
	contract.SourceUserChat:    4,
}

func rankOf(s contract.RoleSource) (int, bool) {
	r, ok := assignmentRank[s]
	return r, ok
}

// RoleError 分工变更被拒绝或需要用户澄清。
type RoleError struct {
	WorkID string
	Reason string
	// NeedUser 为 true 表示必须问用户，不能自行决定。
	NeedUser bool
}

func (e *RoleError) Error() string { return "role: " + e.Reason }

// resolveAssignment 按优先级决定这次分工变更是否生效（§12）。
//
// prev 为当前生效分工（nil 表示还没有）。incoming 是本次候选。
// 返回生效后的分工与"是否发生变化"。
//
// 判定规则（与 README 的判定表一一对应）：
//  1. incoming 来源未知 → 拒绝（宁可问用户，不猜）。
//  2. 没有旧分工 → 直接生效，Version=1。
//  3. incoming 优先级 > prev → 生效，Version=prev.Version+1。
//  4. incoming 优先级 == prev → 同级以最新为准（用户又说了一遍），Version+1。
//  5. incoming 优先级 < prev → 忽略，保持 prev，changed=false。
//     （典型：房间默认/soul 建议不得覆盖用户已明确指定的人）
func resolveAssignment(prev *contract.RoleAssignment, incoming contract.RoleAssignment) (contract.RoleAssignment, bool, error) {
	if _, ok := rankOf(incoming.Source); !ok {
		return contract.RoleAssignment{}, false, &RoleError{
			Reason:   fmt.Sprintf("未知的分工来源 %q", incoming.Source),
			NeedUser: true,
		}
	}
	if strings.TrimSpace(incoming.Executor) == "" {
		return contract.RoleAssignment{}, false, &RoleError{
			Reason:   "执行者不能为空",
			NeedUser: true,
		}
	}

	incoming.Executor = strings.TrimSpace(incoming.Executor)
	incoming.Reviewer = strings.TrimSpace(incoming.Reviewer)
	incoming.Helpers = dedupeIDs(incoming.Helpers)
	incoming.Helpers = removeID(incoming.Helpers, incoming.Executor)
	incoming.Helpers = removeID(incoming.Helpers, incoming.Reviewer)

	if prev == nil {
		incoming.Version = 1
		return incoming, true, nil
	}

	prevRank, _ := rankOf(prev.Source)
	inRank, _ := rankOf(incoming.Source)
	if inRank < prevRank {
		return *prev, false, nil
	}

	incoming.Version = prev.Version + 1
	if sameAssignment(*prev, incoming) {
		// 人没变：仍然记一条版本（用户又明确了一次），但不算"换人"，
		// 因此不撤销待处理项——撤销会误杀正在进行的投递。
		return incoming, false, nil
	}
	return incoming, true, nil
}

// sameAssignment 两份分工的实际内容是否一致（忽略 Version/Source/At）。
func sameAssignment(a, b contract.RoleAssignment) bool {
	if a.Executor != b.Executor || a.Reviewer != b.Reviewer {
		return false
	}
	if len(a.Helpers) != len(b.Helpers) {
		return false
	}
	for i := range a.Helpers {
		if a.Helpers[i] != b.Helpers[i] {
			return false
		}
	}
	return true
}

// validateAssignment 校验分工是否可执行。
//
// 硬规则：执行者与审查者默认不能是同一个 bot（§5.7）。
// 真的只有一个 bot 可用时，不许把自审包装成独立审查：
// 返回 NeedUser=true，由上层转 waiting_user 并明确提示用户。
func validateAssignment(a contract.RoleAssignment) error {
	if strings.TrimSpace(a.Executor) == "" {
		return &RoleError{Reason: "执行者不能为空", NeedUser: true}
	}
	if a.Reviewer != "" && a.Reviewer == a.Executor {
		return &RoleError{
			Reason:   fmt.Sprintf("%s 既是执行者又是审查者，平台不把自审当作独立审查", a.Executor),
			NeedUser: true,
		}
	}
	for _, h := range a.Helpers {
		if h == a.Executor {
			return &RoleError{Reason: fmt.Sprintf("%s 不能同时是执行者与助手", h), NeedUser: true}
		}
	}
	return nil
}

// changeAssignment 落地一次分工变更：写版本记录、撤销旧角色的待处理项（§5.8）。
//
// 撤销只覆盖"尚未产生外部副作用"的投递与审批；已经发进客户端的动作无法回滚，
// 这里如实把它们作为 notice 告诉用户，而不是假装撤销成功。
func (o *Orchestrator) changeAssignment(ctx context.Context, w *contract.Work, incoming contract.RoleAssignment) (revoked []Pending, err error) {
	now := o.now()
	prev := w.Assign
	var prevPtr *contract.RoleAssignment
	if prev.Version > 0 {
		prevPtr = &prev
	}

	next, changed, err := resolveAssignment(prevPtr, incoming)
	if err != nil {
		return nil, err
	}
	next.At = now
	if err := validateAssignment(next); err != nil {
		return nil, err
	}

	// 先写版本记录，再撤销待处理项：顺序反了会在崩溃时丢版本号。
	if err := o.store.SaveAssignment(ctx, w.ID, next); err != nil {
		return nil, err
	}
	w.Assign = next
	touch(w, now)

	if !changed {
		return nil, o.store.SaveWork(ctx, *w)
	}

	pending, err := o.store.PendingFor(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	oldBots := roleBots(prev)
	for _, p := range pending {
		if !containsID(oldBots, p.BotID) {
			continue // 与旧角色无关的待处理项不动
		}
		if !stillInRole(next, p.BotID) {
			if err := o.store.RevokePending(ctx, p.ID, "分工变更"); err != nil {
				return revoked, err
			}
			revoked = append(revoked, p)
		}
	}
	if err := o.store.SaveWork(ctx, *w); err != nil {
		return revoked, err
	}
	return revoked, nil
}

// roleBots 一份分工里出现的所有 bot。
func roleBots(a contract.RoleAssignment) []string {
	out := make([]string, 0, 2+len(a.Helpers))
	if a.Executor != "" {
		out = append(out, a.Executor)
	}
	if a.Reviewer != "" {
		out = append(out, a.Reviewer)
	}
	out = append(out, a.Helpers...)
	return dedupeIDs(out)
}

// stillInRole 该 bot 在新分工里是否仍承担某个角色。
func stillInRole(a contract.RoleAssignment, botID string) bool {
	return a.Executor == botID || a.Reviewer == botID || containsID(a.Helpers, botID)
}

// applyInstruction 把一条用户消息落到 Work 上（§5.8 / §12）。
//
// 返回值 changed 表示 Work 被改动过；notice 是给用户看的一句话（可能为空）。
// 咨询类指令一律不改状态，也不改分工 —— 这是本函数最重要的一个分支。
func (o *Orchestrator) applyInstruction(ctx context.Context, w *contract.Work, text string, mentions []string) (changed bool, notice string, err error) {
	if len(mentions) == 0 {
		// 用户常直接写"改由 doubao 执行"而不打 @，只有一个聊天框时更是如此。
		// 没有 @ 就按 bot 的 ID / 显示名在文本里认人。
		mentions = o.botsInText(text)
	}
	in := interpretRoleInstruction(text, mentions, o.knownBot)

	// 目标模糊：只问模糊的部分，已有委托按原分工继续。
	if len(in.Ambiguous) > 0 {
		return false, "这几个对象我认不出来，没动分工：" + strings.Join(in.Ambiguous, "、") + "。请说清楚谁做什么。", nil
	}

	switch in.Kind {
	case InstrNone, InstrConsult:
		// 咨询不改分工，也不改状态。
		return false, "", nil

	case InstrPause:
		if err := o.pause(ctx, w); err != nil {
			return false, "", err
		}
		return true, "已暂停这个委托。要接着做就说「继续」。", nil

	case InstrResume:
		if err := o.resume(ctx, w); err != nil {
			return false, "", err
		}
		return true, "已继续。", nil

	case InstrCancel:
		if err := o.cancel(ctx, w); err != nil {
			return false, "", err
		}
		return true, o.cancelNotice(w), nil

	case InstrSkipReview:
		next := contract.RoleAssignment{
			Executor: w.Assign.Executor,
			Helpers:  w.Assign.Helpers,
			Source:   contract.SourceUserChat,
		}
		if _, err := o.changeAssignment(ctx, w, next); err != nil {
			return false, "", err
		}
		return true, "已改为不审查：执行者交什么就交付什么，不再等审查。", nil

	case InstrRoleChange:
		next := w.Assign
		next.Source = contract.SourceUserChat
		droppedReviewer := ""
		if in.Executor != "" {
			next.Executor = in.Executor
			// 换执行者时，原来那个人若还是审查者就会变成自审，必须让位。
			// 不能静默保留，也不能静默换个人顶上 —— 告诉用户审查没了。
			if next.Reviewer == in.Executor {
				droppedReviewer = next.Reviewer
				next.Reviewer = ""
			}
		}
		if in.Reviewer != "" {
			next.Reviewer = in.Reviewer
		}
		if next.Reviewer != "" && next.Reviewer == next.Executor {
			return false, in.Reviewer + " 不能同时是执行者和审查者。请指定另一个人审查，或者说「不用审查」。", nil
		}
		if _, err := o.changeAssignment(ctx, w, next); err != nil {
			return false, "", err
		}
		n := describeAssignment(w.Assign)
		if droppedReviewer != "" {
			n += "（" + droppedReviewer + " 原来是审查者，现在是执行者，审查已取消；要换个人审查请说「改由 X 审查」。）"
		}
		return true, n, nil
	}
	return false, "", nil
}

// pause 暂停一个 Work。已经是终态的不动。
func (o *Orchestrator) pause(ctx context.Context, w *contract.Work) error {
	switch w.State {
	case contract.WorkPaused, contract.WorkCompleted, contract.WorkCancelled, contract.WorkFailed:
		return nil
	}
	if err := transition(w, contract.WorkPaused, o.now()); err != nil {
		return err
	}
	return o.store.SaveWork(ctx, *w)
}

// resume 从暂停恢复。恢复到一个"能继续干活"的状态：有审查者且已经审过一轮的
// 回到 executing（要执行者改），否则回到 queued 重新派发。
func (o *Orchestrator) resume(ctx context.Context, w *contract.Work) error {
	if w.State != contract.WorkPaused {
		return nil
	}
	target := contract.WorkQueued
	if w.ReviewRounds > 0 {
		target = contract.WorkExecuting
	}
	if err := transition(w, target, o.now()); err != nil {
		return err
	}
	return o.store.SaveWork(ctx, *w)
}

// cancel 停止一个 Work，并把它的在途外部动作按"能否确认"如实收尾。
//
// 停止不是改个状态就完事：已经发出去的动作必须逐个请求停下，没发出的记为
// failed（已取消），发出去又拿不到回执的留在 unknown（§5.8）。不许直接宣布"已停止"。
func (o *Orchestrator) cancel(ctx context.Context, w *contract.Work) error {
	if w.State == contract.WorkCompleted || w.State == contract.WorkCancelled {
		return nil
	}
	o.stopEffects(w.ID)
	if err := transition(w, contract.WorkCancelled, o.now()); err != nil {
		return err
	}
	o.sched.ForgetWork(w.ID)
	o.guards.ForgetWork(w.ID)
	return o.store.SaveWork(ctx, *w)
}

// stopEffects 对该 Work 每个还没落定的外部动作发出停止请求。
func (o *Orchestrator) stopEffects(workID string) {
	for _, e := range o.effects.ForWork(workID) {
		switch e.State {
		case contract.OpPrepared, contract.OpExecuting, contract.OpUnknown:
			o.StopEffect(workID, e.Key.Step)
		}
	}
}

// cancelNotice 停止之后要告诉用户什么。有未确认的外部动作时，必须说"停止待确认"。
func (o *Orchestrator) cancelNotice(w *contract.Work) string {
	unknown := 0
	for _, e := range o.effects.ForWork(w.ID) {
		if e.State == contract.OpUnknown {
			unknown++
		}
	}
	switch unknown {
	case 0:
		return "已停止这个委托。"
	case 1:
		return StopPendingNotice
	default:
		return "停止待确认：有 " + strconv.Itoa(unknown) + " 个已经发出去的动作无法确认是否真的停下。请到对应客户端窗口确认。"
	}
}

// describeAssignment 把当前分工说成人话。
func describeAssignment(a contract.RoleAssignment) string {
	exec := a.Executor
	if exec == "" {
		exec = "未指定"
	}
	if a.Reviewer == "" {
		return "现在由 " + exec + " 执行，不设审查。"
	}
	return "现在由 " + exec + " 执行，" + a.Reviewer + " 审查。"
}

// ---- 聊天里分工指令的识别 ----

// InstructionKind 用户一句话在分工层面的含义。
type InstructionKind string

const (
	InstrNone       InstructionKind = "none"        // 只是普通聊天
	InstrConsult    InstructionKind = "consult"     // 咨询：@B 看看这个 —— 不改分工
	InstrRoleChange InstructionKind = "role_change" // 改分工：改由 B 审查
	InstrSkipReview InstructionKind = "skip_review" // 明确不要审查
	InstrPause      InstructionKind = "pause"       // 暂停这件事
	InstrResume     InstructionKind = "resume"      // 继续
	InstrCancel     InstructionKind = "cancel"      // 停止，不做了
)

// RoleInstruction 从一条用户消息里解析出的分工意图。
type RoleInstruction struct {
	Kind     InstructionKind
	Executor string
	Reviewer string
	// ClearReviewer 用户明确表示不需要审查者（与"没指定审查者"不同）。
	ClearReviewer bool
	// Ambiguous 需要向用户澄清的目标（不存在/重名/重复）。非空时只问这部分，
	// 已有委托保持原分工继续跑，不因为一句模糊的话就换人。
	Ambiguous []string
	// KeepExisting 表示这次指令不影响已有委托的分工（咨询即属此类）。
	KeepExisting bool
}

// roleChangeVerbs 明确表示"改分工"的说法（§5.8）。
// 只有命中这些词才算改分工；单纯 @ 某人一律按咨询处理。
var roleChangeVerbs = []string{
	"改由", "换成", "改为由", "让", "交给", "由", "负责审查", "来审查", "审查改为", "执行改为",
}

// skipReviewPhrases 用户明确放弃审查的说法。
var skipReviewPhrases = []string{"不用审查", "不需要审查", "跳过审查", "免审查", "直接给我"}

// controlCommands 暂停 / 继续 / 停止的整句命令（§5.8）。
//
// 只认"整句就是一个命令"的形式，不做子串匹配：像"继续"这种词在普通聊天里
// 太常见（"继续用 codex 吧"、"别停止审查"），子串匹配会把正常对话当成控制指令，
// 后果是误停一个正在跑的委托。宁可漏认，让用户点按钮或再说清楚一点。
var controlCommands = map[string]InstructionKind{
	"暂停":   InstrPause,
	"暂停一下": InstrPause,
	"先暂停":  InstrPause,
	"继续":   InstrResume,
	"继续吧":  InstrResume,
	"接着做":  InstrResume,
	"恢复":   InstrResume,
	"停止":   InstrCancel,
	"停":    InstrCancel,
	"取消":   InstrCancel,
	"别做了":  InstrCancel,
	"不用做了": InstrCancel,
}

// matchControlCommand 整句匹配控制命令。
func matchControlCommand(text string) (InstructionKind, bool) {
	t := strings.TrimSpace(text)
	t = strings.TrimRight(t, "。．.!！~～ ,，")
	t = strings.ReplaceAll(t, " ", "")
	if k, ok := controlCommands[t]; ok {
		return k, true
	}
	return InstrNone, false
}

// interpretRoleInstruction 解析用户消息里的分工意图。
//
// 设计取舍（§5.8）：默认不改分工。@ 某人 + 描述 = 咨询；只有出现明确的改分工动词
// 才动角色。识别不了的、@ 到不存在或重名的人，只就那部分提问，绝不自行替换执行者。
func interpretRoleInstruction(text string, mentions []string, knownBot func(id string) bool) RoleInstruction {
	out := RoleInstruction{Kind: InstrNone}
	t := strings.TrimSpace(text)
	if t == "" {
		return out
	}

	// 先认整句控制命令：暂停 / 继续 / 停止。这类指令不改分工，只改状态。
	if k, ok := matchControlCommand(t); ok {
		out.Kind = k
		return out
	}

	// 再处理"明确不要审查"
	for _, p := range skipReviewPhrases {
		if strings.Contains(t, p) {
			out.Kind = InstrSkipReview
			out.ClearReviewer = true
			return out
		}
	}

	hasVerb := false
	for _, v := range roleChangeVerbs {
		if strings.Contains(t, v) {
			hasVerb = true
			break
		}
	}

	// 检查 @ 目标是否可用：不存在或重名 → 只问这部分
	var valid []string
	for _, m := range dedupeIDs(mentions) {
		if knownBot == nil || knownBot(m) {
			valid = append(valid, m)
		} else {
			out.Ambiguous = append(out.Ambiguous, m)
		}
	}

	if !hasVerb {
		// 没有改分工动词：@ 只表示咨询，分工不变。
		if len(mentions) > 0 {
			out.Kind = InstrConsult
			out.KeepExisting = true
		}
		return out
	}

	out.Kind = InstrRoleChange
	if len(valid) > 1 {
		// 一句话点了多个人又没说清谁干什么 → 问，不猜。
		out.Ambiguous = append(out.Ambiguous, valid...)
		out.KeepExisting = true
		return out
	}
	if len(valid) == 1 {
		if strings.Contains(t, "审查") || strings.Contains(t, "复核") || strings.Contains(t, "验收") {
			out.Reviewer = valid[0]
		} else {
			out.Executor = valid[0]
		}
	}
	return out
}

// ---- 小工具 ----

// botsInText 在文本里认人：返回文本中出现的 bot ID。
//
// 只在用户没打 @ 时兜底。用整词边界，避免 "codex" 命中 "codex-mini" 这类前缀关系；
// 匹配不到的（拼错、写了名字但没这个人）一律不返回，交给上层按"认不出来"提问，
// 绝不猜一个最像的顶上去。
func (o *Orchestrator) botsInText(text string) []string {
	if len(o.bots) == 0 || text == "" {
		return nil
	}
	ids := make([]string, 0, len(o.bots))
	for id := range o.bots {
		ids = append(ids, id)
	}
	// 长的先匹配，短的不再吃掉长 ID 的子串。
	sort.Slice(ids, func(i, j int) bool {
		if len(ids[i]) != len(ids[j]) {
			return len(ids[i]) > len(ids[j])
		}
		return ids[i] < ids[j]
	})
	var out []string
	rest := text
	for _, id := range ids {
		if idx := indexBotWord(rest, id); idx >= 0 {
			out = append(out, id)
			rest = rest[:idx] + strings.Repeat("\x00", len(id)) + rest[idx+len(id):]
		}
	}
	return out
}

// indexBotWord 找 id 在 s 里作为独立词出现的位置。
func indexBotWord(s, id string) int {
	for off := 0; off < len(s); {
		i := strings.Index(s[off:], id)
		if i < 0 {
			return -1
		}
		start := off + i
		end := start + len(id)
		if !isBotWordByte(byteAt(s, start-1)) && !isBotWordByte(byteAt(s, end)) {
			return start
		}
		off = start + 1
	}
	return -1
}

func byteAt(s string, i int) byte {
	if i < 0 || i >= len(s) {
		return 0
	}
	return s[i]
}

// isBotWordByte 判断是否是"能粘在 ID 上"的字符。中文不是，所以"改由doubao执行"
// 也能认出来。
func isBotWordByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '-' || b == '_' || b == '.':
		return true
	}
	return false
}

func dedupeIDs(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func removeID(in []string, id string) []string {
	if id == "" || len(in) == 0 {
		return in
	}
	out := in[:0:0]
	for _, s := range in {
		if s != id {
			out = append(out, s)
		}
	}
	return out
}

func containsID(in []string, id string) bool {
	for _, s := range in {
		if s == id {
			return true
		}
	}
	return false
}

// now 便于测试替换时间源。
func (o *Orchestrator) now() time.Time {
	if o.clock != nil {
		return o.clock()
	}
	return time.Now()
}
