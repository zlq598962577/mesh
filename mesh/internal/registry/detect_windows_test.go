//go:build windows

package registry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这组测试用真实的临时目录验证通配展开与排除规则。
// 不用假的路径字符串：expandCandidate 的行为完全由文件系统决定，
// 假路径测不出"匹配到通配分量之后有没有带着剩余分量继续往下走"。

// mkTree 在临时目录里造出给定的相对文件，返回根目录。
func mkTree(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		full := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("建目录失败 %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatalf("写文件失败 %s: %v", full, err)
		}
	}
	return root
}

// TestExpandCandidateWildcardKeepsTail 是本文件最重要的测试。
//
// 早期实现匹配到通配分量就把目录当成最终结果，丢掉了后面的
// app\ChatGPT.exe，导致 Codex 的 MSIX 路径永远探测不到。
// 这个测试就是钉住那个 bug。
func TestExpandCandidateWildcardKeepsTail(t *testing.T) {
	root := mkTree(t,
		`OpenAI.Codex_26.1106.6628.0_x64__2p2nqsd0c76g0\app\ChatGPT.exe`,
		`OpenAI.Codex_11.0.0.0_x64__2p2nqsd0c76g0\app\ChatGPT.exe`,
		`OpenAI.Codex_99.0.0.0_x64__2p2nqsd0c76g0\app\Other.exe`,
	)
	pattern := filepath.Join(root, `OpenAI.Codex_*\app\ChatGPT.exe`)
	got := expandCandidate(pattern)

	if len(got) != 2 {
		t.Fatalf("期望展开出 2 个文件，实际 %d 个：%v", len(got), got)
	}
	for _, g := range got {
		if !strings.HasSuffix(strings.ToLower(g), `app\chatgpt.exe`) {
			t.Errorf("展开结果丢了 app\\ChatGPT.exe 尾巴：%s", g)
		}
	}
	// 结果必须排序，保证"取第一个"是确定性的。
	if got[0] > got[1] {
		t.Errorf("展开结果未排序：%v", got)
	}
}

// TestExpandCandidateNoWildcard 不含通配的路径原样返回，存在性由调用方判断。
func TestExpandCandidateNoWildcard(t *testing.T) {
	root := mkTree(t, `app\Doubao.exe`)
	got := expandCandidate(filepath.Join(root, `app\Doubao.exe`))
	if len(got) != 1 {
		t.Fatalf("期望 1 个结果，实际 %v", got)
	}
}

// TestExpandCandidateWildcardNoMatch 通配没命中时必须返回空，不许返回模式串本身。
func TestExpandCandidateWildcardNoMatch(t *testing.T) {
	root := mkTree(t, `app\Doubao.exe`)
	got := expandCandidate(filepath.Join(root, `Nope_*\app\Doubao.exe`))
	if len(got) != 0 {
		t.Fatalf("无命中时期望空结果，实际 %v", got)
	}
}

// TestMatchWildcardSegment 分量内通配的大小写不敏感匹配。
func TestMatchWildcardSegment(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*", "anything", true},
		{"OpenAI.Codex_*", "openai.codex_26.1_x64", true}, // 大小写不敏感
		{"OpenAI.Codex_*", "OpenAI.Codex_", true},
		{"OpenAI.Codex_*", "Other.Codex_1", false},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "aXcYb", false},
		{"exact.exe", "EXACT.EXE", true},
		{"exact.exe", "exact.dll", false},
	}
	for _, c := range cases {
		if got := matchWildcardSegment(c.pattern, c.name); got != c.want {
			t.Errorf("matchWildcardSegment(%q, %q) = %v，期望 %v", c.pattern, c.name, got, c.want)
		}
	}
}

// TestIsExcludedPath 排除目录必须同时拦住"目录本身"和"目录下的文件"。
//
// 这两种写法都要命中：exclude_paths 写的是目录，实际比较的往往是文件。
func TestIsExcludedPath(t *testing.T) {
	m := &Manifest{
		Detect: DetectSpec{
			ExcludePaths: []string{`F:\AIWORKSPACE\CodexChinese`},
		},
	}
	cases := []struct {
		path string
		want bool
	}{
		{`F:\AIWORKSPACE\CodexChinese`, true},
		{`F:\AIWORKSPACE\CodexChinese\`, true},
		{`F:\AIWORKSPACE\CodexChinese\codex.exe`, true},
		{`f:\aiworkspace\codexchinese\app\x.exe`, true}, // 大小写不敏感
		{`F:\AIWORKSPACE\CodexChinese2\x.exe`, false},   // 前缀不能误伤同名前缀目录
		{`F:\AIWORKSPACE\other\x.exe`, false},
		{`F:\AIWORKSPACE\CodexChinese-备份\x.exe`, false},
	}
	for _, c := range cases {
		if got := isExcludedPath(m, c.path); got != c.want {
			t.Errorf("isExcludedPath(%q) = %v，期望 %v", c.path, got, c.want)
		}
	}
}

// TestIsExcludedPathNilManifest 空 manifest 不许崩。
func TestIsExcludedPathNilManifest(t *testing.T) {
	if isExcludedPath(nil, `C:\x\y.exe`) {
		t.Error("nil manifest 不应判定为排除")
	}
	if isExcludedPath(&Manifest{}, `C:\x\y.exe`) {
		t.Error("空 exclude_paths 不应判定为排除")
	}
}

// TestDetectExcludedBeatsRunning 排除优先于"进程在跑"。
//
// 这是安全语义：即使 F:\AIWORKSPACE\CodexChinese 正在运行，也必须判不可用，
// 因为那个安装会注册管理员计划任务。
func TestDetectExcludedBeatsRunning(t *testing.T) {
	root := mkTree(t, `codex.exe`)
	m := &Manifest{
		ID: "codex",
		Detect: DetectSpec{
			ExePaths:     []string{filepath.Join(root, `codex.exe`)},
			ProcessNames: []string{"codex.exe"},
			ExcludePaths: []string{root},
		},
	}
	d := (&WindowsDetector{}).Detect(context.Background(), m)
	if d.Available {
		t.Error("命中 exclude_paths 时必须判为不可用")
	}
	if d.Err != "" {
		t.Errorf("排除不是探测失败，Err 应为空，实际：%s", d.Err)
	}
}

// TestDetectFoundByExePath 只有文件没有进程时也算可用。
func TestDetectFoundByExePath(t *testing.T) {
	root := mkTree(t, `app\Doubao.exe`)
	m := &Manifest{
		ID: "doubao",
		Detect: DetectSpec{
			ExePaths: []string{filepath.Join(root, `app\Doubao.exe`)},
		},
	}
	d := (&WindowsDetector{}).Detect(context.Background(), m)
	if !d.Available {
		t.Fatalf("文件存在时应判为可用，Notes=%v", d.Notes)
	}
	if d.ExePath == "" {
		t.Error("可用时 ExePath 不应为空")
	}
	// ProbedAt 由 Registry 补，探测器留零值是刻意的约定。
	if !d.ProbedAt.IsZero() {
		t.Errorf("探测器不应自己设置 ProbedAt，应由 Registry 统一补：%v", d.ProbedAt)
	}
}

// TestDetectNotFound 全都没有时不应判为可用。
func TestDetectNotFound(t *testing.T) {
	root := t.TempDir()
	m := &Manifest{
		ID: "ghost",
		Detect: DetectSpec{
			ExePaths:     []string{filepath.Join(root, `nope.exe`)},
			ProcessNames: []string{"definitely-not-running-xyz.exe"},
		},
	}
	d := (&WindowsDetector{}).Detect(context.Background(), m)
	if d.Available {
		t.Fatal("不应判为可用")
	}
	if d.Err != "" {
		t.Logf("进程枚举失败导致 Err 非空（合法，表示未确认）：%s", d.Err)
	}
}

// TestDetectNilManifest 空 manifest 必须返回带 Err 的结果而不是 panic。
func TestDetectNilManifest(t *testing.T) {
	d := (&WindowsDetector{}).Detect(context.Background(), nil)
	if d == nil {
		t.Fatal("Detect 必须返回非 nil")
	}
	if d.Err == "" {
		t.Error("nil manifest 应写 Err")
	}
}

// TestDetectCancelledContext 已取消的 context 不许继续探测。
func TestDetectCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := (&WindowsDetector{}).Detect(ctx, &Manifest{ID: "x"})
	if d.Err == "" {
		t.Error("已取消的 context 应写 Err")
	}
}
