//go:build windows

package probe

import (
	"os"
	"path/filepath"
	"strings"
)

// 本文件是「安装与版本」探测的装配层。
//
// 它把 version.go（已知路径定位 + 版本资源）、registry.go（卸载登记项）、
// process.go（进程归属）三块拼成每个客户端的 InstallProbe。
//
// 关键诚实性约定：
//   - 只用显式写死的已知路径，绝不递归扫盘（§9.5）。
//   - 版本优先取注册表登记项，其次 exe 版本资源，都取不到就写 unknown 并说明。
//   - 登录态只输出「存在」或「unknown」，绝不输出凭据正文（§9.4）。

// clientSpec 是一个客户端的静态探测配置。
// 这些值全部来自本机实测，不是猜的。
type clientSpec struct {
	Adapter     string // 与 contract.SessionInfo.Adapter 一致：codex|dsh|trae|doubao
	DisplayName string

	// Roots 是安装根目录候选（有序，命中即停）。
	Roots func() []string
	// Rels 是主程序相对路径候选（有序）。第一个是首选。
	// 「主程序」= 真正在跑、要用 UIA 操作的那个进程的 exe。
	Rels []string
	// VersionRels 是「版本资源可信」的相对路径候选（有序），留空表示与 Rels 相同。
	//
	// 为什么可能与 Rels 不同：本机实测豆包的主程序是 app\Doubao.exe，
	// 但它的版本资源是 Chromium 版本（147.x），应用版本（2.36.8）在启动器
	// Doubao.exe 里。取版本和定主程序是两个不同的问题，不能共用一份清单。
	VersionRels []string
	// Launchers 是启动脚本相对路径，只用于确认安装根目录，绝不代替验收。
	Launchers []string
	// RegistryName 是卸载登记项展示名的匹配子串。
	RegistryName string
	// CredFiles 是相对于已知数据目录的凭据文件候选，只用于判存在性。
	CredFiles []string
	// DataDirs 是该客户端的数据目录候选（用于凭据存在性探测的基准）。
	DataDirs func() []string
}

func specCodex() clientSpec {
	return clientSpec{
		Adapter:     "codex",
		DisplayName: "Codex（ChatGPT 桌面版）",
		Roots:       codexCandidates,
		// MSIX 包内主程序是 app\ChatGPT.exe。WindowsApps 下的执行别名不是主程序。
		Rels:         []string{`app\ChatGPT.exe`, `ChatGPT.exe`},
		RegistryName: "OpenAI.Codex",
		CredFiles:    []string{"auth.json", "config.toml"},
		DataDirs: func() []string {
			return []string{filepath.Join(userHome(), ".codex")}
		},
	}
}

func specDSH() clientSpec {
	return clientSpec{
		Adapter:      "dsh",
		DisplayName:  "DeepSeek Harness",
		Roots:        dshCandidates,
		Rels:         []string{`DeepSeek Harness.exe`},
		Launchers:    []string{`resources\runtime\cli\bin\dsh.cmd`},
		RegistryName: "DeepSeek Harness",
		CredFiles:    nil, // 未知，见 login 探测的降级说明
		DataDirs: func() []string {
			var out []string
			if a := os.Getenv("APPDATA"); a != "" {
				out = append(out, filepath.Join(a, "@deepseek-ai"))
			}
			return out
		},
	}
}

func specTrae() clientSpec {
	return clientSpec{
		Adapter:      "trae",
		DisplayName:  "Trae CN",
		Roots:        traeCandidates,
		Rels:         []string{`Trae CN.exe`},
		Launchers:    []string{`bin\trae-cn.cmd`},
		RegistryName: "TRAE CN",
		CredFiles:    nil,
		DataDirs: func() []string {
			var out []string
			if a := os.Getenv("APPDATA"); a != "" {
				out = append(out, filepath.Join(a, "Trae CN"))
			}
			return out
		},
	}
}

func specDoubao() clientSpec {
	return clientSpec{
		Adapter:     "doubao",
		DisplayName: "豆包",
		Roots:       doubaoCandidates,
		// 主程序是 app\Doubao.exe（本机 8 个在跑的进程全在它下面）。
		Rels: []string{`app\Doubao.exe`, `Doubao.exe`},
		// 版本资源反过来：应用版本在根目录的启动器里，主程序里是 Chromium 版本。
		// 实测根目录 Doubao.exe = 2.36.8（ProductName "Doubao Launcher"，与卸载登记项一致），
		// app\Doubao.exe = 147.0.7727.149（Chromium）。顺序不能跟着 Rels 走。
		VersionRels:  []string{`Doubao.exe`, `app\Doubao.exe`},
		RegistryName: "豆包",
		CredFiles:    nil,
		DataDirs: func() []string {
			var out []string
			if a := os.Getenv("APPDATA"); a != "" {
				out = append(out, filepath.Join(a, "Doubao"))
			}
			if l := os.Getenv("LOCALAPPDATA"); l != "" {
				out = append(out, filepath.Join(l, "Doubao"))
			}
			return out
		},
	}
}

// Specs 返回四个首发客户端的探测配置。
func Specs() []clientSpec {
	return []clientSpec{specCodex(), specDSH(), specTrae(), specDoubao()}
}

// DetectInstall 探测单个客户端的安装与版本。
func DetectInstall(spec clientSpec, procs []ProcInfo) InstallProbe {
	var ip InstallProbe
	ip.State = Unavailable

	// 1. 定位安装根目录：只按已知候选，命中即停。
	root := ""
	for _, r := range spec.Roots() {
		if isExcluded(r) {
			continue
		}
		if isDir(r) || isFile(r) {
			root = r
			break
		}
	}
	if root == "" {
		ip.Reason = "未在已知安装路径中找到该客户端（不递归扫盘，见 §9.5）"
		ip.Login = probeLogin(spec)
		return ip
	}

	ip = describeInstallRoot(root, spec.Rels, spec.Launchers)

	// 2. 版本来源文件：默认与主程序一致；豆包这类「版本在启动器里」的单独给清单。
	verRels := spec.VersionRels
	if len(verRels) == 0 {
		verRels = spec.Rels
	}
	ip.Version, ip.VersionSource, ip.VersionFile = versionFromRels(root, verRels)

	// 3. 版本：注册表登记项优先于 exe 版本资源（登记项是安装器写的，最权威）。
	if e, ok := FindUninstallEntry(spec.RegistryName); ok {
		if e.DisplayVersion != "" {
			ip.Version = e.DisplayVersion
			ip.VersionSource = "卸载登记项 DisplayVersion（" + e.DisplayName + "）"
		}
	}
	// MSIX 客户端的包标识与版本号直接来自带版本的目录名。
	if strings.Contains(strings.ToLower(root), "windowsapps") {
		ip.PackageFamily = filepath.Base(root)
		if ip.Version == "" {
			// 目录名形如 OpenAI.Codex_26.1106.6628.0_x64__2p2nqsd0c76g0
			if v := versionFromMSIXDir(ip.PackageFamily); v != "" {
				ip.Version = v
				ip.VersionSource = "MSIX 包目录名（升级后路径会变，适配器需固定版本范围）"
			}
		}
	}
	if ip.Version == "" {
		ip.Version = "unknown"
		if ip.VersionSource == "" {
			ip.VersionSource = "none"
		}
	}

	// 3. 进程归属：按安装目录归集，而不是按 exe 名（Electron 子进程同名）。
	mine := procsUnderImage(procs, root)
	if len(mine) == 0 && ip.ExePath != "" {
		// 安装目录没匹配到时，退一步用主程序的目录归属。
		mine = procsUnderImage(procs, filepath.Dir(ip.ExePath))
	}
	for _, p := range mine {
		ip.ProcessIDs = append(ip.ProcessIDs, p.PID)
	}

	// 4. 窗口标题：只收该客户端进程拥有的可见窗口。
	for _, w := range EnumTopWindows() {
		for _, p := range mine {
			if w.PID == p.PID && w.Title != "" {
				ip.WindowTitles = append(ip.WindowTitles, w.Title)
				break
			}
		}
	}

	// 5. 登录态。
	ip.Login = probeLogin(spec)

	if len(mine) == 0 {
		ip.Reason = "已安装但当前未运行（未发现归属该安装目录的进程）"
	}
	return ip
}

// versionFromMSIXDir 从 MSIX 目录名里取版本号。
// 输入形如 OpenAI.Codex_26.1106.6628.0_x64__2p2nqsd0c76g0。
func versionFromMSIXDir(dir string) string {
	parts := strings.Split(dir, "_")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// probeLogin 只判断「凭据是否存在」。
//
// §9.4 严格禁止输出凭据正文，也禁止为了判定有效性去解析它。
// 因此：
//   - 找到候选凭据文件 → LoginPresent（"存在"，字面就是存在，不是"有效"）。
//   - 找不到 → LoginUnknown。绝不写 LoginAbsent，因为凭据可能由 Windows
//     凭据管理器或注册表保存，我们并没有枚举完所有位置，"不存在"无法证明。
//   - 永不出 LoginExpired：过期判定必须读凭据正文，被 §9.4 禁止。
func probeLogin(spec clientSpec) LoginProbe {
	if len(spec.CredFiles) == 0 {
		return LoginProbe{
			State:  LoginUnknown,
			Reason: "该客户端的凭据存放位置未知；按 §9.4 不读取凭据正文，故无法判定",
		}
	}
	for _, d := range spec.DataDirs() {
		if d == "" || !isDir(d) {
			continue
		}
		for _, f := range spec.CredFiles {
			p := filepath.Join(d, f)
			if isFile(p) {
				// Evidence 只写文件名，绝不写内容，也不写账号。
				return LoginProbe{
					State:    LoginPresent,
					Evidence: "数据目录内存在凭据文件：" + f,
					Reason:   "存在性不等于有效性；过期判定需读取凭据正文，被 §9.4 禁止，故不输出「过期」",
				}
			}
		}
	}
	return LoginProbe{
		State:  LoginUnknown,
		Reason: "未在已知数据目录中找到凭据文件；凭据可能由系统凭据管理器保存，未枚举全部位置故不判定为「不存在」",
	}
}
