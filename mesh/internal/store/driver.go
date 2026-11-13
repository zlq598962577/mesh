package store

// SQLite 驱动隔离层。
//
// 全仓库只有本文件 import 第三方 SQLite 驱动（COORDINATION.md 层边界约定）：
// 换驱动、升驱动、改 DSN 参数都只改这一个文件，其余文件只依赖 database/sql。
//
// 选 modernc.org/sqlite 而不是 mattn/go-sqlite3 的原因：
//   - 纯 Go 实现，不需要 cgo，因此 CGO_ENABLED=0 go build 能过（§11 单二进制约束）；
//   - 不依赖 gcc / MSVC，Windows 上开箱可构建；
//   - 不引入 golang.org/x/sys 之外的可疑依赖链（本仓库禁止额外系统依赖）。
//
// 代价：纯 Go 版性能低于 cgo 版。单用户自用规模（单机、万级行）下差异不可感知，
// 这个取舍记录在 README。

import (
	"net/url"
	"strings"

	// 注册 "sqlite" 驱动名；只在本文件出现，其它文件不得直接 import。
	_ "modernc.org/sqlite"
)

// driverName 是 database/sql 使用的驱动名。
const driverName = "sqlite"

// buildDSN 构造 SQLite 连接串。
//
// 连接级 PRAGMA 必须写在 DSN 里，而不是 Open 之后用 PRAGMA 语句设置：
// database/sql 的连接池会在任意时刻新建连接，只在"当前连接"上执行的 PRAGMA
// 不会自动应用到新连接。DSN 里的 _pragma 由驱动在每次建连时执行，才能保证
// 每个连接都有 busy_timeout 与外键约束。
//
//   - busy_timeout(5000)：写锁冲突时最多等 5s 再返回 SQLITE_BUSY。
//     写入本来已由单写队列串行化，这个超时只用于兜住 VACUUM INTO 备份、
//     外部工具（sqlite3 CLI、备份脚本）临时持锁等少数场景。
//   - foreign_keys(1)：SQLite 默认关闭外键，必须显式打开，否则 ON DELETE CASCADE
//     与外键约束全是摆设。
func buildDSN(path string) string {
	// 用 file: URI 形式，路径统一走 url.PathEscape，避免 Windows 反斜杠与
	// 特殊字符（空格、#、?）破坏 DSN 解析。
	var b strings.Builder
	b.WriteString("file:")
	b.WriteString(escapeSQLitePath(path))
	b.WriteString("?_pragma=")
	b.WriteString(url.QueryEscape("busy_timeout(5000)"))
	b.WriteString("&_pragma=")
	b.WriteString(url.QueryEscape("foreign_keys(1)"))
	return b.String()
}

// escapeSQLitePath 把本地路径转成 SQLite URI 可接受的路径部分。
//
// 只替换反斜杠与需要转义的字符，不做 filepath.ToSlash 之外的规范化：
// 调用方传入的已经是 filepath.Join 过的绝对路径。
func escapeSQLitePath(path string) string {
	// Windows 路径用正斜杠，SQLite URI 在 Windows 上同样接受正斜杠。
	p := strings.ReplaceAll(path, `\`, "/")
	// 逐段转义，保留分隔符 "/"，否则整个路径会被转成一个文件名。
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
