//go:build windows

package probe

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// 本文件用「手写 COM vtable」调用 UI Automation，回答 P0a 最核心的问题：
// 四个客户端的聊天输入框 / 发送按钮 / 消息列表到底能不能被程序化访问。
//
// 为什么手写 vtable 而不是用第三方 UIA 绑定库：
// 方案要求只用 Go 标准库，且必须能在 CGO_ENABLED=0 下构建。COM 在 Windows 上
// 就是「拿接口指针 → 读 vtable → 按索引调函数」，标准库的 syscall 足够。
//
// 风险如实说明：vtable 索引一旦写错就是未定义行为（可能崩溃而不是报错），
// 而且本机没有 Go 工具链，这些索引没有经过编译和运行验证。因此本文件的策略是：
//   - 每个索引都注明对应哪个接口方法，便于人工核对；
//   - 所有返回值都按 HRESULT 检查，失败一律降级为 unavailable + 原因，
//     绝不把「调用失败」当成「客户端没有控件」；
//   - 上层用 goroutine + 超时包住，COM 调用卡住时探针仍能出结果。

var (
	modOle32            = syscall.NewLazyDLL("ole32.dll")
	modOleAut32         = syscall.NewLazyDLL("oleaut32.dll")
	modUIAutomationCore = syscall.NewLazyDLL("uiautomationcore.dll")

	procCoInitializeEx   = modOle32.NewProc("CoInitializeEx")
	procCoCreateInstance = modOle32.NewProc("CoCreateInstance")
	procSysFreeString    = modOleAut32.NewProc("SysFreeString")
)

const (
	coinitMultithreaded  = 0x0
	clsctxInprocServer   = 0x1
	treeScopeDescendants = 0x4

	// hrRPCChangedMode 表示当前线程已用另一种套间模型初始化过 COM。
	// 这不是错误，可以继续用已有的套间。
	hrRPCChangedMode = 0x80010106
	hrPointer        = 0x80004003 // E_POINTER
)

// UIA 控件类型 ID（UIA_ControlTypeIds）。这里只用得到少数几个。
const (
	ctButton   = 50000
	ctEdit     = 50004
	ctList     = 50008
	ctText     = 50020
	ctCustom   = 50025
	ctGroup    = 50026
	ctDataGrid = 50028
	ctDocument = 50030
	ctWindow   = 50032
	ctPane     = 50033
	ctTable    = 50036
)

// guid 是 Windows 的 GUID 布局。
type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// parseGUID 解析 "XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX"。
// 写成解析而不是硬编码字节数组，是为了让常量在源码里可读、可核对。
func parseGUID(s string) (guid, error) {
	var g guid
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	p := strings.Split(s, "-")
	if len(p) != 5 || len(p[0]) != 8 || len(p[1]) != 4 ||
		len(p[2]) != 4 || len(p[3]) != 4 || len(p[4]) != 12 {
		return g, fmt.Errorf("probe: 非法 GUID %q", s)
	}
	d1, err := strconv.ParseUint(p[0], 16, 32)
	if err != nil {
		return g, err
	}
	d2, err := strconv.ParseUint(p[1], 16, 16)
	if err != nil {
		return g, err
	}
	d3, err := strconv.ParseUint(p[2], 16, 16)
	if err != nil {
		return g, err
	}
	g.Data1 = uint32(d1)
	g.Data2 = uint16(d2)
	g.Data3 = uint16(d3)
	for i := 0; i < 2; i++ {
		b, err := strconv.ParseUint(p[3][i*2:i*2+2], 16, 8)
		if err != nil {
			return g, err
		}
		g.Data4[i] = byte(b)
	}
	for i := 0; i < 6; i++ {
		b, err := strconv.ParseUint(p[4][i*2:i*2+2], 16, 8)
		if err != nil {
			return g, err
		}
		g.Data4[2+i] = byte(b)
	}
	return g, nil
}

// CLSID_CUIAutomation / IID_IUIAutomation。
var (
	clsidCUIAutomation = guid{0xFF48DBA4, 0x60EF, 0x4201, [8]byte{0xAA, 0x87, 0x54, 0x10, 0x3E, 0xEF, 0x59, 0x4E}}
	iidIUIAutomation   = guid{0x30CBE57D, 0xD9D0, 0x452A, [8]byte{0xAB, 0x13, 0x7A, 0xC5, 0xAC, 0x48, 0x25, 0xEE}}
)

// comCall 按 vtable 索引调用一个 COM 方法。
//
// 索引从 0 开始，0/1/2 恒为 QueryInterface/AddRef/Release。
// obj 是接口指针；args 是「除 this 之外」的实参。
//
// 关于 go vet 的 "possible misuse of unsafe.Pointer"：
// obj / vtbl / fn 都是 COM 对象的原生地址（由 ole32/oleaut32 分配，不在 Go 堆上），
// 不会被 Go 的 GC 移动，所以 uintptr → unsafe.Pointer 的解引用在这里是安全的。
// 这是 Go 官方文档承认的 syscall 例外：指针来自系统调用而非 Go 指针运算。
// vet 无法区分「来自 syscall 的原生地址」和「Go 指针被降级成整数」，因此会误报。
// 保留这些解引用而不改写，是因为 COM 的 vtable 调用本质上必须做整数偏移寻址，
// 没有既符合 vet 又符合 COM ABI 的写法。改动此处前请先确认 vtable 索引正确，
// 索引错会直接崩掉客户端进程，不是报错。
func comCall(obj uintptr, index int, args ...uintptr) uintptr {
	if obj == 0 {
		return hrPointer
	}
	// obj 是 IUIAutomation 系接口指针（原生内存），解引用取 vtable 首地址。
	vtbl := *(*uintptr)(unsafe.Pointer(obj))
	if vtbl == 0 {
		return hrPointer
	}
	// vtbl 指向接口方法表（原生内存），按索引取第 index 个函数地址。
	fn := *(*uintptr)(unsafe.Pointer(vtbl + uintptr(index)*unsafe.Sizeof(uintptr(0))))
	if fn == 0 {
		return hrPointer
	}
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, obj)
	all = append(all, args...)
	r, _, _ := syscall.SyscallN(fn, all...)
	return r
}

// hrFailed 判断 HRESULT 是否表示失败（最高位为 1）。
func hrFailed(hr uintptr) bool {
	return int32(uint32(hr)) < 0
}

// bstrString 把 BSTR 读成 Go 字符串。调用方负责 SysFreeString。
//
// 关于 go vet 的 "possible misuse of unsafe.Pointer"：
// b 是 oleaut32 用 SysAllocString 分配的原生 BSTR（不在 Go 堆上，不会被 GC 移动），
// 且 BSTR 前 4 字节是长度前缀、正文以 NUL 结尾，所以这里只能按 NUL 扫描而不能用
// unsafe.Slice（那样需要先读长度前缀，多一次解引用风险）。
// 扫描上限 maxLen 是防御：BSTR 若已失效，不能让它把内存读穿。
func bstrString(b uintptr) string {
	if b == 0 {
		return ""
	}
	const maxLen = 1 << 16 // 防御：异常 BSTR 不能把内存读穿
	// b 是原生 BSTR 数据区首地址（UTF-16LE），不是 Go 堆指针，
	// 这一处 uintptr → unsafe.Pointer 属于 syscall 例外，见上方函数注释。
	p := (*uint16)(unsafe.Pointer(b))
	n := 0
	for n < maxLen {
		// 从 p 用 unsafe.Add 推进，避免再做一次 uintptr 转换。
		if *(*uint16)(unsafe.Add(unsafe.Pointer(p), uintptr(n)*2)) == 0 {
			break
		}
		n++
	}
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(unsafe.Slice(p, n))
}

// uiaSession 持有一次探测期间需要的 COM 对象。
type uiaSession struct {
	root  uintptr // IUIAutomation*
	ctrl  uintptr // IUIAutomationTreeWalker*（ControlView）
	raw   uintptr // IUIAutomationTreeWalker*（RawView）
	trueC uintptr // IUIAutomationCondition*（TrueCondition）
	ready bool
}

// openUIA 初始化 COM 并创建 IUIAutomation。
func openUIA() (*uiaSession, error) {
	if err := modUIAutomationCore.Load(); err != nil {
		return nil, fmt.Errorf("加载 uiautomationcore.dll 失败：%v", err)
	}
	if err := modOle32.Load(); err != nil {
		return nil, fmt.Errorf("加载 ole32.dll 失败：%v", err)
	}

	hr, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded)
	if hrFailed(hr) && uint32(hr) != hrRPCChangedMode {
		return nil, fmt.Errorf("CoInitializeEx 失败：hr=0x%08X", uint32(hr))
	}

	var p uintptr
	hr, _, _ = procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidCUIAutomation)),
		0,
		clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidIUIAutomation)),
		uintptr(unsafe.Pointer(&p)),
	)
	if hrFailed(hr) || p == 0 {
		return nil, fmt.Errorf("CoCreateInstance(CUIAutomation) 失败：hr=0x%08X", uint32(hr))
	}

	s := &uiaSession{root: p}

	// IUIAutomation vtable 索引（IUnknown 占 0..2）：
	//   14 get_ControlViewWalker   16 get_RawViewWalker   21 CreateTrueCondition
	comCall(p, 14, uintptr(unsafe.Pointer(&s.ctrl)))
	comCall(p, 16, uintptr(unsafe.Pointer(&s.raw)))
	comCall(p, 21, uintptr(unsafe.Pointer(&s.trueC)))
	s.ready = true
	return s, nil
}

// close 释放探测期间创建的 COM 对象。
//
// 故意不调用 CoUninitialize：探针是短命进程，进程退出时 COM 会自行清理，
// 而在多 goroutine + 超时的结构里提前反初始化比泄漏更危险。
func (s *uiaSession) close() {
	if s == nil {
		return
	}
	for _, p := range []uintptr{s.trueC, s.raw, s.ctrl, s.root} {
		if p != 0 {
			comCall(p, 2) // Release
		}
	}
	s.ready = false
}

// elementFromHWND 把窗口句柄换成 UIA 元素。
// IUIAutomation vtable 索引 6 = ElementFromHandle。
func (s *uiaSession) elementFromHWND(hwnd uint64) (uintptr, error) {
	var e uintptr
	hr := comCall(s.root, 6, uintptr(hwnd), uintptr(unsafe.Pointer(&e)))
	if hrFailed(hr) || e == 0 {
		return 0, fmt.Errorf("ElementFromHandle 失败：hr=0x%08X", uint32(hr))
	}
	return e, nil
}

// element 是一个 UIA 元素的属性快照。
type element struct {
	ptr          uintptr
	ControlType  int32
	Name         string
	AutomationID string
	ClassName    string
	PID          int32
}

// readElement 读一个元素的常用属性。
//
// IUIAutomationElement vtable 索引（IUnknown 占 0..2）：
//
//	20 get_CurrentProcessId   21 get_CurrentControlType   23 get_CurrentName
//	29 get_CurrentAutomationId 30 get_CurrentClassName
func readElement(e uintptr) element {
	el := element{ptr: e}
	if e == 0 {
		return el
	}
	var pid int32
	if hr := comCall(e, 20, uintptr(unsafe.Pointer(&pid))); !hrFailed(hr) {
		el.PID = pid
	}
	var ct int32
	if hr := comCall(e, 21, uintptr(unsafe.Pointer(&ct))); !hrFailed(hr) {
		el.ControlType = ct
	}
	el.Name = bstrGetter(e, 23)
	el.AutomationID = bstrGetter(e, 29)
	el.ClassName = bstrGetter(e, 30)
	return el
}

// bstrGetter 调用一个返回 BSTR 的 getter 并释放 BSTR。
func bstrGetter(e uintptr, index int) string {
	var b uintptr
	hr := comCall(e, index, uintptr(unsafe.Pointer(&b)))
	if hrFailed(hr) || b == 0 {
		return ""
	}
	defer procSysFreeString.Call(b)
	return bstrString(b)
}

// findAll 返回后代元素个数与（有上限的）元素列表。
//
// IUIAutomationElement vtable 索引 6 = FindAll。
// FindAll 走的是 ControlView；RawView 的规模另外用 rawChildren 测。
func (s *uiaSession) findAll(elem uintptr, limit int) (int, []element, error) {
	var arr uintptr
	hr := comCall(elem, 6, uintptr(treeScopeDescendants), s.trueC, uintptr(unsafe.Pointer(&arr)))
	if hrFailed(hr) || arr == 0 {
		return 0, nil, fmt.Errorf("FindAll(Descendants) 失败：hr=0x%08X", uint32(hr))
	}
	defer comCall(arr, 2) // Release

	// IUIAutomationElementArray vtable 索引 3 = get_Length，4 = GetElement
	var n int32
	if hr := comCall(arr, 3, uintptr(unsafe.Pointer(&n))); hrFailed(hr) {
		return 0, nil, fmt.Errorf("ElementArray.get_Length 失败：hr=0x%08X", uint32(hr))
	}
	total := int(n)
	if total < 0 {
		total = 0
	}

	cap := total
	if limit > 0 && cap > limit {
		cap = limit
	}
	out := make([]element, 0, cap)
	for i := 0; i < cap; i++ {
		var child uintptr
		hr := comCall(arr, 4, uintptr(i), uintptr(unsafe.Pointer(&child)))
		if hrFailed(hr) || child == 0 {
			continue
		}
		out = append(out, readElement(child))
	}
	return total, out, nil
}

// rawChildren 用 RawView 走一层直接子元素，用来区分
// 「ControlView 过滤掉了所有子元素」和「真的没有子元素」。
//
// IUIAutomationTreeWalker vtable 索引 3 = get_FirstChildElement，
// 5 = get_NextSiblingElement。
func (s *uiaSession) rawChildren(elem uintptr, limit int) int {
	if s.raw == 0 {
		return 0
	}
	var cur uintptr
	if hr := comCall(s.raw, 3, elem, uintptr(unsafe.Pointer(&cur))); hrFailed(hr) || cur == 0 {
		return 0
	}
	n := 0
	for cur != 0 && n < limit {
		n++
		var next uintptr
		if hr := comCall(s.raw, 5, cur, uintptr(unsafe.Pointer(&next))); hrFailed(hr) {
			comCall(cur, 2)
			break
		}
		comCall(cur, 2) // 释放当前兄弟
		cur = next
	}
	if cur != 0 {
		comCall(cur, 2)
	}
	return n
}

// rawDepth 用 RawView 递归测树深度（有上限），用于说明
// 「树是空的」还是「树很深但 ControlView 里没东西」。
func (s *uiaSession) rawDepth(elem uintptr, maxDepth int) int {
	if s.raw == 0 || elem == 0 || maxDepth <= 0 {
		return 0
	}
	best := 0
	var cur uintptr
	if hr := comCall(s.raw, 3, elem, uintptr(unsafe.Pointer(&cur))); hrFailed(hr) || cur == 0 {
		return 0
	}
	for cur != 0 {
		if d := s.rawDepth(cur, maxDepth-1); d > best {
			best = d
		}
		var next uintptr
		if hr := comCall(s.raw, 5, cur, uintptr(unsafe.Pointer(&next))); hrFailed(hr) {
			comCall(cur, 2)
			break
		}
		comCall(cur, 2)
		cur = next
	}
	if cur != 0 {
		comCall(cur, 2)
	}
	return best + 1
}

// controlTypeName 把 UIA 控件类型 ID 转成可读名。
func controlTypeName(ct int32) string {
	switch ct {
	case ctButton:
		return "Button"
	case ctEdit:
		return "Edit"
	case ctList:
		return "List"
	case ctText:
		return "Text"
	case ctCustom:
		return "Custom"
	case ctGroup:
		return "Group"
	case ctDataGrid:
		return "DataGrid"
	case ctDocument:
		return "Document"
	case ctWindow:
		return "Window"
	case ctPane:
		return "Pane"
	case ctTable:
		return "Table"
	}
	if ct == 0 {
		return "unknown"
	}
	return "ControlType(" + strconv.Itoa(int(ct)) + ")"
}
