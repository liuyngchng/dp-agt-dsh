package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 系统配置
type Config struct {
	Server ServerConfig `yaml:"server"`
	Sys    SysConfig    `yaml:"sys"`
	LLM    LLMConfig    `yaml:"llm"`
}

type ServerConfig struct {
	Port     int    `yaml:"port"`
	SelfPort int    `yaml:"self_port"`
	Debug    bool   `yaml:"debug"`
	SelfIP   string `yaml:"self_ip"`
}

type SysConfig struct {
	MaxConcurrentTasks int `yaml:"max_concurrent_tasks"`
	MaxPendingTasks    int `yaml:"max_pending_tasks"`
	RetentionDays      int `yaml:"retention_days"`
	DataDir            string
}

type LLMConfig struct {
	BaseURL                   string `yaml:"base_url"`
	AuthToken                 string `yaml:"auth_token"`
	Model                     string `yaml:"model"`
	TimeoutMs                 int    `yaml:"timeout_ms"`
	DisableNonessentialTraffic bool   `yaml:"disable_nonessential_traffic"`

	// dsh 内部字段（不从 yaml 读取）
	Provider    string `yaml:"-"` // deepseek-official
	DSHome      string `yaml:"-"` // dsh 运行时 home 目录
	RuntimeDir  string `yaml:"-"` // dsh 子进程进程级工作目录（initialize.cwd）
}

// RawConfig 用于获取 cfg.yml 的原始 map 供 CLAUDE.md 模板渲染
func LoadRaw(dataDir string) (map[string]interface{}, error) {
	paths := []string{
		dataDir + "/cfg.yml",
		"data/cfg.yml",
	}
	var raw map[string]interface{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
		return raw, nil
	}
	return raw, nil // 允许不存在
}

func Load(dataDir string) (*Config, error) {
	raw, err := LoadRaw(dataDir)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		raw = make(map[string]interface{})
	}

	cfg := &Config{}

	// server
	cfg.Server.Port = intMap(raw, "server.port", 20002)
	cfg.Server.SelfPort = intMap(raw, "server.self_port", 20003)
	cfg.Server.Debug = boolMap(raw, "server.debug", false)
	cfg.Server.SelfIP = strMap(raw, "server.self_ip", "")

	// sys
	cfg.Sys.MaxConcurrentTasks = intMap(raw, "sys.max_concurrent_tasks", 5)
	cfg.Sys.MaxPendingTasks = intMap(raw, "sys.max_pending_tasks", 10)
	cfg.Sys.RetentionDays = intMap(raw, "sys.retention_days", 30)
	cfg.Sys.DataDir = strMap(raw, "sys.data_dir", "data")

	// env override
	if v := os.Getenv("DATA_DIR"); v != "" {
		cfg.Sys.DataDir = v
	}
	if v := os.Getenv("SERVER_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = n
		}
	}
	if v := os.Getenv("MAX_CONCURRENT_TASKS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Sys.MaxConcurrentTasks = n
		}
	}
	if v := os.Getenv("MAX_PENDING_TASKS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Sys.MaxPendingTasks = n
		}
	}

	// llm
	cfg.LLM.BaseURL = strMap(raw, "llm.base_url", "")
	cfg.LLM.AuthToken = strMap(raw, "llm.auth_token", "")
	cfg.LLM.Model = strMap(raw, "llm.model", "deepseek-v4-pro")
	cfg.LLM.TimeoutMs = intMap(raw, "llm.timeout_ms", 300000)
	cfg.LLM.DisableNonessentialTraffic = boolMap(raw, "llm.disable_nonessential_traffic", true)

	// 环境变量覆盖（dsh 原生使用 DEEPSEEK_BASE_URL / DEEPSEEK_API_KEY）
	if v := os.Getenv("DEEPSEEK_BASE_URL"); v != "" {
		cfg.LLM.BaseURL = v
	}
	if v := os.Getenv("DEEPSEEK_API_KEY"); v != "" {
		cfg.LLM.AuthToken = v
	}
	if v := os.Getenv("DEEPSEEK_MODEL"); v != "" {
		cfg.LLM.Model = v
	}

	// dsh 特有默认值
	cfg.LLM.Provider = "deepseek-official"
	cfg.LLM.DSHome = os.Getenv("DSH_HOME")
	if cfg.LLM.DSHome == "" {
		cfg.LLM.DSHome = dataDir + "/dsh-home"
	}
	cfg.LLM.RuntimeDir = cfg.DSHRuntimeDir()

	// 校验
	if cfg.LLM.BaseURL == "" {
		// dsh 可以直接连 DeepSeek 官方，不需要 base_url
		// 但如果有自定义网关，必须配置
	}

	return cfg, nil
}

// RuntimeTasksDir 返回会话工作目录根目录（本地运行与容器内统一约定，不落在 data/ 挂载卷内）。
// 以 DATA_DIR 所在位置为基准：DATA_DIR=/app/data（容器）→ /app/runtime/tasks；
// DATA_DIR=data（本地）→ runtime/tasks。
func (c *Config) RuntimeTasksDir() string {
	if dir := strings.TrimSpace(os.Getenv("RUNTIME_TASKS_DIR")); dir != "" {
		return dir
	}
	abs, err := filepath.Abs(c.Sys.DataDir)
	if err != nil {
		abs = c.Sys.DataDir
	}
	return filepath.Join(filepath.Dir(abs), "runtime", "tasks")
}

// DSHRuntimeDir 返回 dsh 子进程的进程级兜底工作目录（Session 诞生时的初始 cwd）。
// 它一定存在且可读写，仅作为 session 创建后再切换到真实会话目录之前的过渡目录。
func (c *Config) DSHRuntimeDir() string {
	if dir := strings.TrimSpace(os.Getenv("DSH_RUNTIME_DIR")); dir != "" {
		return dir
	}
	return c.RuntimeTasksDir()
}

// RawConfig 返回原始 map（给 CLAUDE.md 模板用）
func (c *Config) RawConfig() map[string]interface{} {
	return map[string]interface{}{
		"server": map[string]interface{}{
			"port":      c.Server.Port,
			"self_port": c.Server.SelfPort,
			"self_ip":   c.Server.SelfIP,
		},
		"sys": map[string]interface{}{
			"max_concurrent_tasks": c.Sys.MaxConcurrentTasks,
			"max_pending_tasks":    c.Sys.MaxPendingTasks,
			"retention_days":       c.Sys.RetentionDays,
		},
		"llm": map[string]interface{}{
			"base_url":  c.LLM.BaseURL,
			"model":     c.LLM.Model,
			"provider":  c.LLM.Provider,
		},
	}
}

// ── 辅助 ──

func strMap(raw map[string]interface{}, path, def string) string {
	v := dig(raw, path)
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func intMap(raw map[string]interface{}, path string, def int) int {
	v := dig(raw, path)
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return def
}

func boolMap(raw map[string]interface{}, path string, def bool) bool {
	v := dig(raw, path)
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

func dig(m map[string]interface{}, path string) interface{} {
	parts := splitPath(path)
	var cur interface{} = m
	for _, p := range parts {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func splitPath(p string) []string {
	var parts []string
	s := ""
	for _, c := range p {
		if c == '.' {
			parts = append(parts, s)
			s = ""
		} else {
			s += string(c)
		}
	}
	if s != "" {
		parts = append(parts, s)
	}
	return parts
}