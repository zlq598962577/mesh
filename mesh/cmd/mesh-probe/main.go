//go:build windows

// mesh-probe 探测本机四个桌面客户端（Codex / DSH / Trae / 豆包）的可接入能力。
//
// ┌──────────────────────────────────────────────────────────────────────┐
// │ 关于退出码，请先读这一段。                                            │
// │                                                                      │
// │ 本程序的退出码只表示「这次探测本身跑完没跑完」，                       │
// │ 与「客户端能不能用」没有任何关系。                                    │
// │                                                                      │
// │   0  探测正常跑完，所有检查项都得到了结论（结论本身可能是不可用）      │
// │   1  探测跑完了，但其中有检查项失败或无法得出结论                     │
// │   2  参数错误，探测没有开始                                           │
// │                                                                      │
// │ 换句话说：退出码 0 完全不代表客户端可用。客户端可用性只看结果里的     │
// │ state 字段（verified / unverified / unavailable）和汇总表。           │
// │                                                                      │
// │ 这条规矩不是本程序自己的发明。方案 §4.3 line 242 明确要求 successSignal │
// │ 必填、不能只靠 exit code——实测依据是 DSH 在模型调用失败时进程退出码   │
// │ 依然是 0。如果连被测程序都这样，探测程序自己更不能拿退出码当结论。     │
// └──────────────────────────────────────────────────────────────────────┘
//
// 用法：
//
//	mesh-probe                    人读表格，全部客户端
//	mesh-probe -json              结构化 JSON，全部客户端
//	mesh-probe -client dsh        只探测 DSH
//	mesh-probe -client all -json  等价于 -json
//	mesh-probe -timeout 30s       总超时 30 秒
//
// 超时行为：总超时到点后，已经拿到的结果照常输出，没探测完的客户端
// 标为 unavailable 并写明「超时未完成」。程序不会挂死，也不会假装完成。
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"mesh/internal/probe"
)

// 退出码。含义见文件头注释。
const (
	exitOK       = 0
	exitProbeBad = 1
	exitUsage    = 2
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		jsonOut  = flag.Bool("json", false, "输出结构化 JSON 而不是人读表格")
		client   = flag.String("client", "all", "要探测的客户端：codex | dsh | trae | doubao | all，可用逗号分隔多个")
		timeout  = flag.Duration("timeout", probe.DefaultTimeout, "整次探测的总超时（到点后返回已得结果，未完成项标 unknown）")
		listOnly = flag.Bool("list", false, "只列出可探测的客户端名后退出")
	)
	flag.Usage = usage
	flag.Parse()

	if *listOnly {
		for _, s := range probe.Specs() {
			fmt.Printf("%-8s %s\n", s.Adapter, s.DisplayName)
		}
		return exitOK
	}

	// ---- 参数校验（失败一律 exit 2，不进入探测）----
	clients, err := parseClients(*client)
	if err != nil {
		fmt.Fprintf(os.Stderr, "参数错误：%v\n\n", err)
		flag.Usage()
		return exitUsage
	}
	if *timeout <= 0 {
		fmt.Fprintf(os.Stderr, "参数错误：-timeout 必须为正数，收到 %v\n", *timeout)
		return exitUsage
	}
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "参数错误：不接受位置参数 %q\n\n", flag.Args())
		flag.Usage()
		return exitUsage
	}

	// ---- 探测 ----
	res := probe.Run(probe.Options{
		Clients: clients,
		Timeout: *timeout,
	})

	// ---- 输出 ----
	if *jsonOut {
		if err := probe.WriteJSON(os.Stdout, res); err != nil {
			fmt.Fprintf(os.Stderr, "写出 JSON 失败：%v\n", err)
			return exitProbeBad
		}
	} else {
		probe.WriteTable(os.Stdout, res)
	}

	// ---- 退出码：只看探测本身是否完整，不看客户端是否可用 ----
	sums := probe.Summarize(res)
	incomplete := false
	for _, s := range sums {
		if s.Verdict == probe.VerdictUnusable || s.Verdict == probe.VerdictUnknown {
			incomplete = true
		}
	}
	if len(res.Blockers) > 0 {
		incomplete = true
	}
	if incomplete {
		return exitProbeBad
	}
	return exitOK
}

// parseClients 解析 -client 参数。
//
// 只接受已知的适配器名，写错就报错——而不是静默忽略。
// 静默忽略会让人以为「探测过了但没问题」，实际是根本没探测。
func parseClients(s string) ([]string, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "all" {
		return nil, nil // nil = 全部
	}

	known := make(map[string]bool)
	for _, spec := range probe.Specs() {
		known[spec.Adapter] = true
	}

	var out []string
	seen := make(map[string]bool)
	for _, part := range strings.Split(s, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if name == "all" {
			return nil, nil
		}
		if !known[name] {
			var valid []string
			for _, spec := range probe.Specs() {
				valid = append(valid, spec.Adapter)
			}
			return nil, fmt.Errorf("未知客户端 %q；可选值：%s、all",
				name, strings.Join(valid, "、"))
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-client 未包含任何有效客户端名")
	}
	return out, nil
}

// usage 打印用法。
func usage() {
	fmt.Fprintf(os.Stderr, `mesh-probe —— 探测本机四个桌面客户端的可接入能力

用法：
  mesh-probe [选项]

选项：
  -json            输出结构化 JSON 而不是人读表格
  -client NAME     要探测的客户端：codex | dsh | trae | doubao | all（默认 all，可用逗号分隔多个）
  -timeout DUR     整次探测的总超时（默认 %v）
  -list            只列出可探测的客户端名后退出

退出码（只表示探测是否跑完，不表示客户端能否使用）：
  0  探测跑完，所有检查项都得到了结论
  1  探测跑完，但有检查项失败或无法得出结论
  2  参数错误，探测未开始

注意：退出码 0 不代表客户端可用。可用性请看输出里的 state 字段与汇总表。
`, probe.DefaultTimeout)
}
