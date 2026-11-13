package app

// 配置加载与目录约定（task-7 / §9.2 L0）。
//
// 本文件只做三件事：解析启动参数、把路径解析成绝对真实路径、把目录建出来。
// 不读业务配置（bot.yaml 由 registry 负责），不碰数据库，不碰网络。
//
// 两条硬红线写在这里，别处不许绕开：
//  1. 临时文件一律落 F:\AIWORKSPACE\temp\mesh，绝不允许落 C 盘。
//     解析出来的临时目录如果在 C 盘，启动直接失败，不做"尽力而为"的降级。
//  2. 所有对外可见的路径（data_dir、workspace、temp_dir）必须是绝对真实路径：
//     已 Clean、已 Abs、已解掉符号链接与 junction。上层拿到的字符串就是磁盘上那一个目录。
//
// 目录约定（第一次启动自动创建，之后只校验不覆盖）：
//
//	<data_dir>/                默认 F:\AIWORKSPACE\mesh-data
//	  mesh.db                  store.DBPath(data_dir) 算出来的库文件（含 -wal / -shm）
//	  agents/                  registry.AgentsDir()，用户层 agent manifest
//	  bots/                    bot 配置（JSON 语法，后缀 .json 或 .yaml）
//	  backups/                 store.Backup 的落点
//	  logs/                    运行日志
//	  workspaces/<bot_id>/     workspace.dir = auto 时每个 bot 的私有工作区
//
// 这些名字是内部约定，改动等于改部署形态，写进 mesh/README.md。

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 默认与固定路径。这两个常量是产品决定，不是"可配置项"。
const (
	// DefaultDataDir 默认数据目录。放在 F 盘是硬要求，不是偏好。
	DefaultDataDir = `F:\AIWORKSPACE\mesh-data`

	// FixedTempDir 平台自有临时目录。所有子进程的 TEMP/TMP 都会被指到这里。
	FixedTempDir = `F:\AIWORKSPACE\temp\mesh`

	// DefaultListen 默认只监听回环。空端口由系统分配，启动后打印真实地址。
	DefaultListen = "127.0.0.1:0"

	// DefaultRoomID / DefaultRoomTitle 首版只有一个房间（单聊天框）。
	DefaultRoomID    = "room-main"
	DefaultRoomTitle = "主房间"
)

// 目录名常量，避免各处硬编码字符串走样。
const (
	dirAgents     = "agents"
	dirBots       = "bots"
	dirBackups    = "backups"
	dirLogs       = "logs"
	dirWorkspaces = "workspaces"
)

// Config 是组装入口需要的全部外部输入。
//
// 只放"启动时确定、运行期不再变"的东西。运行期可变状态在 App 上，不在 Config 上。
type Config struct {
	// DataDir 数据目录。所有持久化状态都在它下面。
	DataDir string
	// TempDir 平台临时目录。固定值，允许覆盖只为了测试，生产不走这条路径。
	TempDir string
	// Listen 本机服务的监听地址。只允许回环地址，非回环在 Resolve 里被拒。
	Listen string
	// RoomID / RoomTitle 首版单房间。
	RoomID    string
	RoomTitle string

	// 以下字段由 Resolve 从 DataDir 派生，不接受外部直接设置。
	BotsDir       string
	AgentsDir     string
	BackupsDir    string
	LogsDir       string
	WorkspacesDir string
	DBPath        string

	// MaxActiveRuns 同时活跃的 Run 上限（§5.6 允许 2 个）。0 表示用默认值 2。
	MaxActiveRuns int
	// ShutdownGrace 优雅退出的总预算。0 表示用默认值 20s。
	ShutdownGrace time.Duration
	// CredentialFile 本机访问凭据的落盘位置。空表示放在 <data_dir>/credential。
	CredentialFile string
}

// DefaultConfig 返回一份可直接用的默认配置（路径尚未解析）。
func DefaultConfig() Config {
	return Config{
		DataDir:       DefaultDataDir,
		TempDir:       FixedTempDir,
		Listen:        DefaultListen,
		RoomID:        DefaultRoomID,
		RoomTitle:     DefaultRoomTitle,
		MaxActiveRuns: 2,
		ShutdownGrace: 20 * time.Second,
	}
}

// ParseArgs 解析命令行参数。
//
// 返回值即使出错也返回一份已填默认值的 Config，方便调用方在错误信息里带上上下文。
// 参数表刻意保持很小：首版没有"调参"需求，多一个开关就多一个能被配错的地方。
func ParseArgs(args []string, stderr io.Writer) (Config, error) {
	cfg := DefaultConfig()
	if stderr == nil {
		stderr = io.Discard
	}

	fs := flag.NewFlagSet("mesh", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "mesh —— 多 Agent 协作平台本机服务\n\n用法: mesh [参数]\n\n参数:\n")
		fs.PrintDefaults()
	}

	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "数据目录（默认 "+DefaultDataDir+"）")
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "本机服务监听地址，只接受回环地址")
	fs.StringVar(&cfg.RoomID, "room", cfg.RoomID, "房间 id（首版单房间）")
	fs.StringVar(&cfg.RoomTitle, "room-title", cfg.RoomTitle, "房间标题")
	fs.IntVar(&cfg.MaxActiveRuns, "max-active-runs", cfg.MaxActiveRuns, "同时活跃 Run 上限")
	fs.DurationVar(&cfg.ShutdownGrace, "shutdown-grace", cfg.ShutdownGrace, "优雅退出预算")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if rest := fs.Args(); len(rest) > 0 {
		return cfg, fmt.Errorf("app: 不认识的参数 %q（本程序只接受上面列出的参数）", strings.Join(rest, " "))
	}
	return cfg, nil
}

// Resolve 把 Config 里的路径全部解析成绝对真实路径，并派生目录字段。
//
// 必须在 EnsureDirs 之前调用。Resolve 自身不创建任何东西，便于"只检查不落地"。
func (c *Config) Resolve() error {
	if c == nil {
		return errors.New("app: 配置为空")
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return errors.New("app: data_dir 不能为空")
	}
	if strings.TrimSpace(c.TempDir) == "" {
		c.TempDir = FixedTempDir
	}
	if c.MaxActiveRuns <= 0 {
		c.MaxActiveRuns = 2
	}
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = 20 * time.Second
	}
	if strings.TrimSpace(c.RoomID) == "" {
		return errors.New("app: room 不能为空")
	}

	dataDir, err := ResolveDir(c.DataDir)
	if err != nil {
		return fmt.Errorf("app: 解析 data_dir 失败: %w", err)
	}
	tempDir, err := ResolveDir(c.TempDir)
	if err != nil {
		return fmt.Errorf("app: 解析 temp_dir 失败: %w", err)
	}

	// 红线：临时目录不许在 C 盘。宁可启动失败，也不许悄悄写到系统盘。
	if IsSystemDrive(tempDir) {
		return fmt.Errorf("app: temp_dir 解析到系统盘 %q，拒绝启动（临时文件一律放 F 盘，默认 %s）", tempDir, FixedTempDir)
	}
	// 临时目录也不许落在数据目录内部：备份/清理时容易互相误伤。
	if isWithin(tempDir, dataDir) || isWithin(dataDir, tempDir) {
		return fmt.Errorf("app: temp_dir %q 与 data_dir %q 不能互相包含", tempDir, dataDir)
	}

	c.DataDir = dataDir
	c.TempDir = tempDir
	c.BotsDir = filepath.Join(dataDir, dirBots)
	c.AgentsDir = filepath.Join(dataDir, dirAgents)
	c.BackupsDir = filepath.Join(dataDir, dirBackups)
	c.LogsDir = filepath.Join(dataDir, dirLogs)
	c.WorkspacesDir = filepath.Join(dataDir, dirWorkspaces)
	c.DBPath = filepath.Join(dataDir, "mesh.db")
	if strings.TrimSpace(c.CredentialFile) == "" {
		c.CredentialFile = filepath.Join(dataDir, "credential")
	}

	if err := ValidateListen(c.Listen); err != nil {
		return err
	}
	return nil
}

// EnsureDirs 创建目录约定里列出的全部目录。
//
// 已存在不算错（幂等）。权限 0o755：这是单用户本机服务，没有多用户隔离诉求，
// 但凭据文件单独用 0o600 写，见 app.go。
func (c *Config) EnsureDirs() error {
	if c == nil {
		return errors.New("app: 配置为空")
	}
	dirs := []string{
		c.DataDir, c.TempDir, c.BotsDir, c.AgentsDir,
		c.BackupsDir, c.LogsDir, c.WorkspacesDir,
	}
	for _, d := range dirs {
		if d == "" {
			return errors.New("app: 目录约定不完整，请先调用 Resolve")
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("app: 创建目录 %s 失败: %w", d, err)
		}
	}
	return nil
}

// BotWorkspace 返回某个 bot 的私有工作区目录（workspace.mode = private）。
//
// botID 必须已经过 sanitizeBotID 校验，本函数不做二次清洗——
// 因为它返回的路径会被直接建目录，双重清洗反而掩盖上层的校验漏洞。
func (c *Config) BotWorkspace(botID string) string {
	return filepath.Join(c.WorkspacesDir, botID)
}

// ChildEnv 返回给平台自生子进程用的环境变量。
//
// 关键点：TEMP/TMP/TMPDIR 一律改写到 c.TempDir。
// 不改的话，客户端 helper 会把中间产物写进 C 盘用户临时目录，违反红线。
// 保留原有环境变量（PATH、代理等），只覆盖临时目录三项。
func (c *Config) ChildEnv(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	override := map[string]string{
		"TEMP":  c.TempDir,
		"TMP":   c.TempDir,
		"TMPDIR": c.TempDir,
	}
	out := make([]string, 0, len(base)+len(override))
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if ok {
			if _, drop := override[strings.ToUpper(name)]; drop {
				continue
			}
		}
		out = append(out, kv)
	}
	// 固定顺序，便于日志比对。
	for _, k := range []string{"TEMP", "TMP", "TMPDIR"} {
		out = append(out, k+"="+override[k])
	}
	return out
}

// ResolveDir 把一个路径解析成绝对真实路径。
//
// 处理四件事：
//  1. 相对路径 → 相对当前工作目录的绝对路径。
//  2. Clean：消掉 "."、".." 与重复分隔符（路径穿越在字符串层面就被抹平）。
//  3. 已存在的部分用 EvalSymlinks 解掉符号链接与 junction（Windows 上 junction 也走这条）。
//  4. 尚不存在的尾部原样拼回，保证"第一次启动目录还不存在"也能解析。
//
// 返回的字符串一定满足 filepath.IsAbs，且不含 "." / ".." 段。
func ResolveDir(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("路径为空")
	}
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("无法转成绝对路径: %w", err)
	}
	real, err := resolveExistingPrefix(abs)
	if err != nil {
		return "", err
	}
	real = filepath.Clean(real)
	if !filepath.IsAbs(real) {
		return "", fmt.Errorf("解析结果不是绝对路径: %q", real)
	}
	for _, seg := range strings.FieldsFunc(real, func(r rune) bool {
		return r == '\\' || r == '/'
	}) {
		if seg == ".." {
			return "", fmt.Errorf("解析结果仍含上跳段: %q", real)
		}
	}
	return real, nil
}

// resolveExistingPrefix 对 p 的最长存在前缀做 EvalSymlinks，再把不存在的尾巴接回去。
func resolveExistingPrefix(p string) (string, error) {
	if _, err := os.Lstat(p); err == nil {
		real, evalErr := filepath.EvalSymlinks(p)
		if evalErr != nil {
			return "", fmt.Errorf("解符号链接失败 %q: %w", p, evalErr)
		}
		return real, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		// 权限不足等：不猜，直接报错。猜出来的路径不可信。
		return "", fmt.Errorf("无法检查路径 %q: %w", p, err)
	}

	parent := filepath.Dir(p)
	if parent == p {
		// 已经到根，说明整条链都不存在（例如盘符不存在），按原样返回。
		return p, nil
	}
	realParent, err := resolveExistingPrefix(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(p)), nil
}

// IsSystemDrive 判断路径是否落在系统盘（通常是 C:）。
//
// 只比较卷名，不比较盘符大小写差异。系统盘从 SystemDrive 环境变量取，取不到就退化为 C:。
func IsSystemDrive(p string) bool {
	vol := strings.ToUpper(filepath.VolumeName(p))
	if vol == "" {
		return false // 非盘符形式（UNC / 相对）不在此判定范围
	}
	sys := strings.ToUpper(os.Getenv("SystemDrive"))
	if sys == "" {
		sys = "C:"
	}
	return vol == sys
}

// isWithin 判断 child 是否等于 parent 或位于 parent 之下（大小写不敏感，Windows 语义）。
func isWithin(child, parent string) bool {
	if child == "" || parent == "" {
		return false
	}
	child = strings.ToLower(filepath.Clean(child))
	parent = strings.ToLower(filepath.Clean(parent))
	if child == parent {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

// ValidateListen 校验监听地址只指向回环。
//
// 本机服务绝不允许监听 0.0.0.0：那等于把整个协作平台（含可执行工具调用）暴露到局域网。
// 端口允许为 0（由系统分配），也允许为空（等价 :0，但主机必须是回环）。
func ValidateListen(addr string) error {
	host, port, err := splitHostPort(addr)
	if err != nil {
		return err
	}
	if !IsLoopbackHost(host) {
		return fmt.Errorf("app: listen 只允许回环地址，收到 %q（可用 127.0.0.1 或 localhost）", addr)
	}
	if port < 0 || port > 65535 {
		return fmt.Errorf("app: listen 端口越界: %d", port)
	}
	return nil
}

// splitHostPort 把 "host:port" 拆开，同时接受空主机与纯端口写法。
func splitHostPort(addr string) (string, int, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", 0, errors.New("app: listen 不能为空")
	}
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return "", 0, fmt.Errorf("app: listen 缺少端口: %q", addr)
	}
	host := addr[:idx]
	portStr := addr[idx+1:]
	if portStr == "" {
		return "", 0, fmt.Errorf("app: listen 端口为空: %q", addr)
	}
	port := 0
	for _, r := range portStr {
		if r < '0' || r > '9' {
			return "", 0, fmt.Errorf("app: listen 端口非法: %q", portStr)
		}
		port = port*10 + int(r-'0')
		if port > 65535 {
			return "", 0, fmt.Errorf("app: listen 端口越界: %q", portStr)
		}
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return host, port, nil
}

// IsLoopbackHost 判断主机名是否是回环。
//
// 只认 127.0.0.0/8、::1、localhost 与空主机（空主机等价于"所有地址"，
// 但这里配合 listen 语义按回环处理会在绑定阶段暴露，所以空主机单独拒绝）。
func IsLoopbackHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "localhost", "::1", "0:0:0:0:0:0:0:1":
		return true
	}
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	if parts[0] != "127" {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}
