package contract

// EngineType 引擎通道类型（§4.2）。
//
// 首版只有 desktop 是真实通道：它驱动用户已安装的 Windows 客户端。
// builtin 与 cli 是未来可选通道，不得用来替代四个桌面客户端的接入目标。
type EngineType string

const (
	EngineDesktop EngineType = "desktop"
	EngineBuiltin EngineType = "builtin" // 未来可选
	EngineCLI     EngineType = "cli"     // 未来可选
)

// EngineSpec 对应 bot.yaml 的 engine 段。
type EngineSpec struct {
	Type        EngineType `yaml:"type" json:"type"`
	Adapter     string     `yaml:"adapter" json:"adapter"`             // codex | dsh | trae | doubao
	Connection  string     `yaml:"connection" json:"connection"`       // auto
	SessionMode string     `yaml:"session_mode" json:"session_mode"`   // dedicated
	Model       string     `yaml:"model" json:"model,omitempty"`       // 分层控制，可留空
}

// Budget 单 bot 预算（§5.3 / §9.6）。
// daily_tokens 只对能可靠计量 token 的通道生效；桌面客户端首版不计 token。
type Budget struct {
	DailyTokens     int64 `yaml:"daily_tokens" json:"daily_tokens"`
	MaxStepsPerTurn int   `yaml:"max_steps_per_turn" json:"max_steps_per_turn"`
	MaxTurnSeconds  int   `yaml:"max_turn_seconds" json:"max_turn_seconds"`
}

// Wake 唤醒开关（§5.2）。on_room_message 默认 false，是 token 放大器。
type Wake struct {
	OnMention     bool   `yaml:"on_mention" json:"on_mention"`
	OnDM          bool   `yaml:"on_dm" json:"on_dm"`
	OnRoomMessage bool   `yaml:"on_room_message" json:"on_room_message"`
	Schedule      string `yaml:"schedule" json:"schedule,omitempty"`
}

// Workspace 工作区（§9.2 L0）。Dir 解析后必须是绝对真实路径，
// 已处理路径穿越、符号链接与 junction。
type Workspace struct {
	Mode string `yaml:"mode" json:"mode"` // private
	Dir  string `yaml:"dir" json:"dir"`   // auto 或绝对路径
}

// MemorySpec 记忆配置（§6）。P0 不做 embedding，recall 退化为近期消息 + 笔记关键词。
type MemorySpec struct {
	LongTerm   bool `yaml:"long_term" json:"long_term"`
	RecallTopK int  `yaml:"recall_top_k" json:"recall_top_k"`
}

// BotSpec 对应一个 bot.yaml 的完整内容（§4.1）。
// 这是 Tier-1 声明式 manifest 的 bot 侧契约；agent 注册表另有 manifest 契约。
type BotSpec struct {
	ID        string     `yaml:"id" json:"id"`
	Name      string     `yaml:"name" json:"name"`
	Role      string     `yaml:"role" json:"role"`
	Soul      string     `yaml:"soul" json:"soul"`
	Engine    EngineSpec `yaml:"engine" json:"engine"`
	Tools     []string   `yaml:"tools" json:"tools"`
	Workspace Workspace  `yaml:"workspace" json:"workspace"`
	Memory    MemorySpec `yaml:"memory" json:"memory"`
	Budget    Budget     `yaml:"budget" json:"budget"`
	Wake      Wake       `yaml:"wake" json:"wake"`
}
