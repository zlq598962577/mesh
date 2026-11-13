// mesh —— 多 Agent 协作平台的本机服务入口（task-7）。
//
// 职责只有三件事：解析参数、按依赖顺序装配（app.Build）、把生命周期交给
// app.Lifecycle 管理。所有业务逻辑都在 internal/* 里，这个文件不许长出第二个。
//
// 运行形态：双击或命令行启动后常驻，只监听 127.0.0.1 回环，用户用浏览器打开
// 控制台 URL（带一次性 ?t= 凭据）操作。Ctrl+C 触发优雅退出（宽限内按 LIFO 收尾：
// 先停 HTTP 关 SSE，再关库；第二个 Ctrl+C 放弃宽限立即退）。
//
// P0 诚实边界：bot 配置文件（<data_dir>/bots/*.json）的加载与真实适配器注入
// 属于适配器阶段接线；本入口此时以"零 bot"状态启动，界面会如实显示能力受限，
// 不会伪装出可用的 agent。
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"

	"mesh/internal/app"
)

func main() {
	cfg, err := app.ParseArgs(os.Args[1:], os.Stderr)
	if err != nil {
		// ParseArgs 自己会把用法打到 stderr；这里只补退出码语义。
		os.Exit(2)
	}
	if err := cfg.Resolve(); err != nil {
		failSlow("启动参数错误", err, 2)
		return
	}

	lc := app.NewLifecycle(cfg.ShutdownGrace, func(format string, args ...any) {
		fmt.Printf("[mesh] "+format+"\n", args...)
	})
	lc.WatchSignals()

	a, err := app.Build(lc.Context(), cfg, app.BuildOptions{AutoLoad: true})
	if err != nil {
		failSlow("启动失败", err, 1)
		return
	}
	// 装配期发现的问题（坏配置、适配器缺失等）在启动横幅里如实列出。
	for _, note := range a.Notes {
		fmt.Println("[mesh] 注意:", note)
	}

	// LIFO 收尾顺序：先注册的在最后执行。关库必须排第一（最后执行），
	// 因为 web 收尾期间桥接层还可能写状态。
	lc.OnShutdown("store", func(context.Context) error { return a.Store.Close() })
	lc.OnShutdown("web", func(ctx context.Context) error { return a.Web.Shutdown(ctx) })

	go func() {
		if err := a.Run(lc.Context()); err != nil {
			lc.Stop("HTTP 服务异常退出: " + err.Error())
		} else {
			lc.Stop("运行结束")
		}
	}()

	if err := lc.Wait(); err != nil {
		fmt.Fprintln(os.Stderr, "退出完成，但部分收尾步骤失败:", err)
		os.Exit(1)
	}
}

// failSlow 报告启动失败并等待用户按回车再退出。
//
// 双击运行的场景：控制台程序报错后立即退出，窗口一闪而过，用户只看到
// "启动不起来"却永远看不到原因。等一次回车，错误信息就看得见了。
// 命令行 / 管道运行时 stdin 已关闭，读入立即返回，不会卡住自动化。
func failSlow(what string, err error, code int) {
	fmt.Fprintln(os.Stderr, what+":", err)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "常见原因：数据目录没有写权限（默认 F:\\AIWORKSPACE\\mesh-data）。")
	fmt.Fprintln(os.Stderr, "换一个可写目录启动的示例：")
	fmt.Fprintln(os.Stderr, `    mesh.exe -data-dir D:\mesh-data`)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprint(os.Stderr, "按回车关闭…")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(code)
}
