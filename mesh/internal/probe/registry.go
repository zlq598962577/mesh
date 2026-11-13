//go:build windows

package probe

import (
	"strings"
	"syscall"
)

// 本文件只做一件事：从「已知的卸载登记项」里读出已安装版本。
//
// 为什么要读注册表：§10 要求报告「安装与版本」，而 Electron/MSIX 客户端的
// 可执行文件版本资源常常不可靠——
//   - Codex 是 MSIX，app\ChatGPT.exe 的版本资源为空，真实版本在包目录名里；
//   - DSH 的 exe ProductVersion 是 0.2.2.0，而安装器登记的版本是 0.2.2-rc.1。
// 所以 exe 版本资源只能作为兜底，注册表登记项才是「安装版本」的权威来源。
//
// 这里用的是标准库 syscall 自带的注册表封装，不引入任何第三方依赖，
// 也不需要手写 vtable。读取范围严格限定在固定的 Uninstall 子键下，
// 不递归、不遍历全盘（§9.5）。

// uninstallRoots 是卸载登记项的三个已知位置。
// 32/64 位视图都要看：Trae 这类用户级安装只在 HKCU 下登记。
var uninstallRoots = []struct {
	hive syscall.Handle
	path string
}{
	{syscall.HKEY_CURRENT_USER, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
	{syscall.HKEY_LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
	{syscall.HKEY_LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`},
}

// UninstallEntry 是一条卸载登记项里我们关心的字段。
// 只保留展示名与版本，绝不读取任何凭据相关键值。
type UninstallEntry struct {
	DisplayName     string
	DisplayVersion  string
	InstallLocation string
}

// FindUninstallEntry 按展示名的子串匹配（大小写不敏感）找一条登记项。
// 找不到返回 false——这是合法结果，上层退化为 exe 版本资源。
func FindUninstallEntry(nameSubstr string) (UninstallEntry, bool) {
	needle := strings.ToLower(nameSubstr)
	for _, root := range uninstallRoots {
		entries := enumUninstall(root.hive, root.path)
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.DisplayName), needle) {
				return e, true
			}
		}
	}
	return UninstallEntry{}, false
}

func enumUninstall(hive syscall.Handle, path string) []UninstallEntry {
	var out []UninstallEntry

	rootPath, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil
	}
	var root syscall.Handle
	if err := syscall.RegOpenKeyEx(hive, rootPath, 0, syscall.KEY_READ, &root); err != nil {
		// 权限不足或键不存在都直接跳过：探测不能因为一个键读不到就失败。
		return nil
	}
	defer syscall.RegCloseKey(root)

	// 上限保护：某些机器上 Uninstall 下有几百个子键，设个硬上限避免无限枚举。
	const maxSubKeys = 512
	buf := make([]uint16, 256)
	for i := 0; i < maxSubKeys; i++ {
		nameLen := uint32(len(buf))
		err := syscall.RegEnumKeyEx(root, uint32(i), &buf[0], &nameLen,
			nil, nil, nil, nil)
		if err != nil {
			// ERROR_NO_MORE_ITEMS 或缓冲不足都结束枚举。
			break
		}
		sub := string(syscall.UTF16ToString(buf[:nameLen]))

		subPath, err := syscall.UTF16PtrFromString(path + `\` + sub)
		if err != nil {
			continue
		}
		var key syscall.Handle
		if err := syscall.RegOpenKeyEx(hive, subPath, 0, syscall.KEY_READ, &key); err != nil {
			continue
		}
		e := UninstallEntry{
			DisplayName:     regString(key, "DisplayName"),
			DisplayVersion:  regString(key, "DisplayVersion"),
			InstallLocation: regString(key, "InstallLocation"),
		}
		syscall.RegCloseKey(key)

		if e.DisplayName == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// regStringKnown 按「已知相对路径 + 值名」读取 HKLM 下的字符串值。
//
// 只用于读取系统版本等公开信息；不读取任何凭据相关键值（§9.4）。
// 打开失败返回空串，由调用方决定如何降级。
func regStringKnown(subKey, valueName string) string {
	p, err := syscall.UTF16PtrFromString(subKey)
	if err != nil {
		return ""
	}
	var key syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, p, 0, syscall.KEY_READ, &key); err != nil {
		return ""
	}
	defer syscall.RegCloseKey(key)
	return regString(key, valueName)
}

// regString 读一个 REG_SZ 值，失败返回空串。
func regString(key syscall.Handle, name string) string {
	namep, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return ""
	}
	// 先探长度。
	var typ uint32
	var size uint32
	if err := syscall.RegQueryValueEx(key, namep, nil, &typ, nil, &size); err != nil {
		return ""
	}
	if size == 0 || size > 64*1024 {
		return ""
	}
	buf := make([]byte, size)
	if err := syscall.RegQueryValueEx(key, namep, nil, &typ, &buf[0], &size); err != nil {
		return ""
	}
	switch typ {
	case syscall.REG_SZ, syscall.REG_EXPAND_SZ:
		// UTF-16 字节转字符串。
		u := make([]uint16, 0, size/2)
		for i := 0; i+1 < len(buf); i += 2 {
			c := uint16(buf[i]) | uint16(buf[i+1])<<8
			if c == 0 {
				break
			}
			u = append(u, c)
		}
		return string(syscall.UTF16ToString(u))
	}
	return ""
}
