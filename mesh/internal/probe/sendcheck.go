//go:build windows

package probe

// 本文件回答 P0a 的第 4 个问题：能不能可靠地「把话送进去、把话发出去、
// 知道对方说完了、知道对方出错了」。
//
// 这一项是整个 P0a 里最容易被糊弄过去的地方，所以先把不许做的事写在前面。
//
// 一、禁止「N 秒没有新字符 = 完成」（§4.2.2 line 218 明文禁止）
//
// 理由不是洁癖，是这条判据在真实场景下必然误判：
//   - 模型思考 / 工具调用期间，界面上可能长时间一个字都不变，此时「静默」
//     代表「还在干活」，按静默判完成会把半截结果当成最终结果交付；
//   - 反过来，长回答被拆成多段流式输出时，段与段之间会出现短暂静默，
//     按静默判完成会把同一轮拆成两轮，触发重复投递；
//   - 客户端弹审批框等用户点时，界面同样完全静止，静默无法区分
//     「在思考」和「在等人批准」——而 §4.3 要求审批是独立事件类型。
// 静默时长这个数字无论调成多少都是猜的，没有可验收的依据。
//
// 二、禁止用退出码判断成功（§4.3 line 242）
//
// 实测依据：DSH 在模型调用失败时，进程退出码仍然是 0。
// 也就是说 exit code 反映的是「程序正常退出了」，而不是「这轮任务成功了」。
// 所以 contract.FinalInfo 里 successSignal 是必填字段：必须有客户端自己给出的
// 成功信号（明确的完成标记 / 会话状态 / 结构化事件），才允许报成功。
//
// 三、本探针在测不出可靠信号时的态度
//
// 标 unverified 或 unavailable，并在 Forbidden 里记下「我们拒绝使用哪些替代判据」。
// 宁可交付「这一项没验出来」，也不能交付「用静默猜出来的成功」。

// forbiddenCompletionJudgements 列出被禁止用作完成判据的做法。
// 这份清单会原样写进探测结果的 forbidden 字段，让下游适配器作者看到红线。
func forbiddenCompletionJudgements() []string {
	return []string{
		"「N 秒没有新字符 = 完成」：静默无法区分「在思考」「在等审批」「已结束」，§4.2.2 line 218 明文禁止",
		"「进程退出码 = 0 = 成功」：实测 DSH 模型调用失败时退出码仍为 0，§4.3 line 242 要求 successSignal 必填",
		"「发送按钮点击成功 = 消息已送达」：点击只证明点了，不证明客户端显示了这条请求（§4.2.2 line 217）",
		"「界面出现任何新文字 = 本轮回复」：无法区分旧回复、系统提示与本次回复",
		"「截图/OCR 识别到完成字样 = 完成」：OCR 结果不可靠且无结构，只能作为未验证兜底（§4.2.1 line 207）",
		"「按固定坐标点击 = 已发送」：窗口位置/DPI/版本变化后坐标失效，§11 line 667 禁止盲点坐标",
	}
}

// probeSendCheck 探测发送/接收链路四个环节的可测性。
//
// 四个环节的可测性是有依赖顺序的：
//
//	文本注入  → 需要能访问输入控件（或剪贴板+粘贴）
//	发送触发  → 需要能触发提交动作
//	完成检测  → 需要客户端暴露完成信号
//	失败检测  → 需要错误/限额/重登/审批是独立状态
//
// 前一环不可用时，后一环直接标 unavailable 并说明「依赖前一环」，
// 而不是各自编一个结论。
func probeSendCheck(ip InstallProbe, cp ControlProbe) SendCheckProbe {
	var sc SendCheckProbe
	sc.State = Unavailable

	forbidden := forbiddenCompletionJudgements()

	// 未安装/未运行：整条链路无从谈起。
	if ip.State == Unavailable {
		reason := "客户端未定位到安装或未运行，发送链路无法探测"
		sc.TextInjection = SignalProbe{State: Unavailable, Reason: reason}
		sc.SendTrigger = SignalProbe{State: Unavailable, Reason: reason}
		sc.Completion = CompletionProbe{State: Unavailable, Reason: reason, Forbidden: forbidden}
		sc.Failure = FailureProbe{State: Unavailable, Reason: reason}
		sc.Stop = SignalProbe{State: Unavailable, Reason: reason}
		sc.Reason = reason
		return sc
	}

	// 控件树不可达：这是本机实测的真实结论，必须如实传导下去。
	// 注意措辞：说「探针主机无法枚举控件树」，不说「客户端没有控件」。
	if cp.State != Verified {
		reason := "控件树不可达（" + firstNonEmpty(cp.Reason, "UIA 未能枚举到该窗口的后代元素") + "），" +
			"文本注入与发送触发均无可靠入口；按 §10 记录为阻碍，不以猜测替代"
		sc.TextInjection = SignalProbe{
			State:  Unavailable,
			Method: "UIA ValuePattern.SetValue / TextPattern / 剪贴板粘贴（后者需还原校验，§4.2.2 line 216）",
			Reason: reason,
		}
		sc.SendTrigger = SignalProbe{
			State:  Unavailable,
			Method: "UIA InvokePattern 点击发送按钮，或键盘 Enter（需焦点，§4.2.2 line 215 不得抢焦点）",
			Reason: "依赖文本注入先行可用；当前控件不可达，故未探测",
		}
		sc.Completion = CompletionProbe{
			State: Unavailable,
			Reason: "完成检测需要客户端暴露可靠完成信号；" +
				"控件不可达时无法读取任何会话状态，且禁用静默判据，故无可用信号",
			Forbidden: forbidden,
		}
		sc.Failure = FailureProbe{
			State: Unavailable,
			Reason: "错误/限额/重新登录/弹窗/工具审批需分别识别为独立状态（§4.2.2 line 218），" +
				"控件不可达时无法识别其中任何一类",
		}
		sc.Stop = SignalProbe{
			State:  Unavailable,
			Method: "UIA 点击停止按钮，或客户端提供的取消接口",
			Reason: "控件不可达，停止入口未知；§10.1 line 644 要求客户端未确认停止不得报已停止",
		}
		sc.Reason = "发送链路整体不可测：控件树不可达是前置阻碍"
		return sc
	}

	// 控件可达（当前机器上未出现此分支，但代码路径必须完整）。
	// 即便可达，也只到「入口存在」这一层；完成检测仍缺可靠信号。
	if hasHit(cp, "chat_input") {
		sc.TextInjection = SignalProbe{
			State:  Unverified,
			Method: "UIA ValuePattern.SetValue / TextPattern",
			Reason: "找到疑似输入控件，但未做真实写入验证（探针不向用户会话注入测试文本）",
		}
	} else {
		sc.TextInjection = SignalProbe{
			State:  Unavailable,
			Method: "UIA 控件搜索（Edit / Document 类型）",
			Reason: "控件树可达但未找到输入控件；可能命名不同或为自绘控件",
		}
	}

	if hasHit(cp, "send_button") {
		sc.SendTrigger = SignalProbe{
			State:  Unverified,
			Method: "UIA InvokePattern",
			Reason: "找到疑似发送按钮，但未真实点击验证；" +
				"§4.2.2 line 217 要求发送后必须核对「客户端显示了这条请求」，点击成功不等于已送达",
		}
	} else {
		sc.SendTrigger = SignalProbe{
			State:  Unavailable,
			Method: "UIA 控件搜索（Button 类型）+ 键盘 Enter",
			Reason: "未找到发送按钮；键盘 Enter 需要焦点，与 §4.2.2 line 215 不得抢焦点冲突",
		}
	}

	sc.Completion = CompletionProbe{
		State: Unverified,
		Signals: []string{
			"未在客户端公开接口中找到结构化的「本轮结束」信号",
			"可尝试方向：客户端是否暴露会话状态查询接口；是否有明确的「生成完成」控件状态",
			"以上方向均需人工在真实会话中验证，探针不自行猜测",
		},
		Forbidden: forbidden,
		Reason: "客户端未暴露可靠完成信号时不按「几秒没新字」宣告成功（§4.2.2 line 218）；" +
			"本项未验证，不得据此外推为可用",
	}

	sc.Failure = FailureProbe{
		State: Unverified,
		Signals: []string{
			"错误、限额、重新登录、弹窗、工具审批必须分别识别为独立状态（§4.2.2 line 218）",
			"未实测到这五类状态的可识别特征",
		},
		DistinctStates: false,
		Reason:         "未能在真实会话中触发并区分错误/限额/重登/弹窗/审批五类状态，故不标已验证",
	}

	sc.Stop = SignalProbe{
		State:  Unverified,
		Method: "UIA 点击停止按钮",
		Reason: "停止效果必须验证（§4.4：Cancel 效果必须核实）；未实测，故不标已验证",
	}

	sc.State = Unverified
	sc.Reason = "控件入口存在但均未做真实收发验证；完成/失败/停止三类信号仍缺失"
	return sc
}

// hasHit 判断某个角色是否被标记为已验证命中。
func hasHit(cp ControlProbe, role string) bool {
	for _, h := range cp.Hits {
		if h.Role == role && h.State == Verified {
			return true
		}
	}
	return false
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
