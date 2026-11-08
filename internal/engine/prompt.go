package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"dp-agt-dsh/internal/store"
)

// PromptFile 提示词文件
type PromptFile struct {
	Filename string // 相对路径，如 "doc/classify.md"
	Path     string // 访问路径，如 "doc/classify"
	Name     string // 从文件中提取的标题
	Content  string // 文件内容
}

// ScanPromptsDir 扫描 prompts/ 目录，返回 path → PromptFile 映射
func ScanPromptsDir(baseDir string) (map[string]*PromptFile, []PromptFile) {
	m := make(map[string]*PromptFile)
	var list []PromptFile

	candidates := []string{
		"prompts",
		filepath.Join(baseDir, "..", "prompts"),
		filepath.Join(baseDir, "prompts"),
	}
	for _, dir := range candidates {
		abs, _ := filepath.Abs(dir)
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			continue
		}
		walk(abs, "", m, &list)
		break
	}
	return m, list
}

func walk(dir, prefix string, m map[string]*PromptFile, list *[]PromptFile) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if name[0] == '_' || name == "sections" {
			continue
		}
		full := filepath.Join(dir, name)
		rel := prefix + name
		if entry.IsDir() {
			walk(full, rel+"/", m, list)
		} else if strings.HasSuffix(name, ".md") {
			relPath := strings.TrimSuffix(rel, ".md") // doc/classify
			baseName := strings.TrimSuffix(name, ".md")
			if baseName == "claude-md" {
				continue
			}

			content, _ := os.ReadFile(full)
			text := string(content)
			title := baseName
			if re := regexp.MustCompile(`(?m)^#\s+(.+)`); re.MatchString(text) {
				title = re.FindStringSubmatch(text)[1]
			}

			pf := &PromptFile{Filename: rel, Path: relPath, Name: title, Content: text}
			m[relPath] = pf
			*list = append(*list, *pf)
		}
	}
}

// LoadPromptFromDisk 从 prompts/ 目录读取指定 path 对应的提示词
func LoadPromptFromDisk(path string) string {
	if pf := promptCache[path]; pf != nil {
		return pf.Content
	}
	// 兜底
	filename := path + ".md"
	candidates := []string{
		filepath.Join("prompts", filename),
		filepath.Join("..", "prompts", filename),
	}
	for _, p := range candidates {
		if data, err := os.ReadFile(p); err == nil {
			return string(data)
		}
	}
	return ""
}

var promptCache map[string]*PromptFile

func InitPrompts(dataDir string) {
	m, list := ScanPromptsDir(dataDir)
	promptCache = m
	_ = list
}

// RenderPrompt 渲染提示词：替换 {{request.xxx}} 和 {{task_id}} 占位符
func RenderPrompt(template string, requestBody map[string]interface{}, taskID ...string) string {
	result := template
	if len(taskID) > 0 && taskID[0] != "" {
		result = strings.ReplaceAll(result, "{{task_id}}", taskID[0])
	}
	re := regexp.MustCompile(`\{\{(request\.[^}]+)\}\}`)
	return re.ReplaceAllStringFunc(result, func(match string) string {
		inner := re.FindStringSubmatch(match)[1]
		fieldPath := strings.TrimPrefix(inner, "request.")
		var val interface{}
		if fieldPath == "body" {
			val = requestBody
		} else {
			val = dig(requestBody, fieldPath)
		}
		if val == nil {
			return ""
		}
		if s, ok := val.(string); ok {
			return s
		}
		b, _ := json.MarshalIndent(val, "", "  ")
		return string(b)
	})
}

func dig(obj map[string]interface{}, path string) interface{} {
	parts := strings.Split(path, ".")
	var cur interface{} = obj
	for _, p := range parts {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

// KeyToEnvName 把 prompt_config 的 key 转为 DP_ 前缀环境变量名：点→下划线，全大写。
// 与 dp-agt 的 keyToEnvName 语义一致。
func KeyToEnvName(key string) string {
	return "DP_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// BuildBusinessEnv 根据 prompt_config（全量明文）构建注入 dsh 子进程的业务环境变量。
// 除 DP_ 前缀变量外，还保留几个兼容旧名，供提示词中 $FILE_URL_TOKEN 等直接引用。
func BuildBusinessEnv(cfgVals map[string]string) map[string]string {
	env := make(map[string]string, len(cfgVals)+3)
	for k, v := range cfgVals {
		env[KeyToEnvName(k)] = v
	}
	if v := cfgVals["doc.file_url_token"]; v != "" {
		env["FILE_URL_TOKEN"] = v
	}
	if v := cfgVals["doc_audit.token"]; v != "" {
		env["DOC_AUDIT_TOKEN"] = v
	}
	if v := cfgVals["callback.token"]; v != "" {
		env["CALLBACK_TOKEN"] = v
	}
	return env
}

// BuildClaudeMd 构建 CLAUDE.md 并把它写入工作目录
func BuildClaudeMd(workDir string, s *store.Store, rawCfg map[string]interface{}, dataDir string) error {
	// 加载主模板
	template := loadFile("claude-md.md")
	if template == "" {
		template = defaultClaudeMd()
	}

	// 收集所有值
	cfgVals := s.GetAllConfigPlain()

	// 系统级
	if rawCfg != nil {
		if server, ok := rawCfg["server"].(map[string]interface{}); ok {
			if port, ok := server["port"]; ok {
				cfgVals["server.port"] = fmt.Sprintf("%v", port)
			}
			if ip, ok := server["self_ip"]; ok {
				cfgVals["server.self_ip"] = fmt.Sprintf("%v", ip)
			}
		}
	}

	// 加载所有 sections
	sections := loadAllFiles()

	// {{section:name:trigger_key}}（name 允许含连字符，如 workflow-transfer）
	reSection := regexp.MustCompile(`\{\{section:([\w-]+):([\w.]+)\}\}`)
	template = reSection.ReplaceAllStringFunc(template, func(match string) string {
		parts := reSection.FindStringSubmatch(match)
		name := parts[1]
		triggerKey := parts[2]
		if _, ok := cfgVals[triggerKey]; !ok {
			return ""
		}
		for key, content := range sections {
			if key == "sections/"+name+".md" || key == name+".md" {
				return content
			}
		}
		return ""
	})

	// 注：{{key}} 形式的明文占位符已废弃。所有配置值一律通过 DP_* 环境变量注入，
	// 模板中直接写 $DP_XXX 由 Agent 在 shell 中按需读取，不再渲染明文进提示词。

	// 注入业务知识记忆库索引
	if dataDir == "" {
		dataDir = "data"
	}
	memItems := ListMemories(dataDir)
	if len(memItems) > 0 {
		// 记忆库目录转成绝对路径，避免 dsh 以 workdir 为基准解析相对路径时找不到文件
		memAbs, err := filepath.Abs(MemoryDir(dataDir))
		if err != nil {
			memAbs = MemoryDir(dataDir)
		}
		var b strings.Builder
		b.WriteString("\n## 业务知识记忆库\n\n")
		b.WriteString("以下是可用的业务背景知识文件，均位于 `" + memAbs + "` 目录。")
		b.WriteString("当任务需要相关业务背景知识时，")
		b.WriteString("使用 Read 工具读取对应文件（路径见下表）。\n\n")
		b.WriteString("| 文件 | 说明 |\n")
		b.WriteString("|------|------|\n")
		for _, item := range memItems {
			desc := item.Description
			if desc == "" {
				desc = item.Title
			}
			b.WriteString("| " + filepath.Join(memAbs, item.Filename) + " | " + desc + " |\n")
		}
		template += b.String()
	}

	// 注入可用环境变量清单（值不写入提示词，仅列出变量名 + 说明），
	// 与 dp-agt 行为一致，便于 Agent 按需读取 token 等变量而不必盲猜。
	if rows := s.ListConfigRows(false); len(rows) > 0 {
		var b strings.Builder
		b.WriteString("\n## 可用环境变量\n\n")
		b.WriteString("以下变量已注入到当前 shell 环境，可在 Bash 中按需读取（如 `echo $变量名`）。")
		b.WriteString("变量名以 `DP_` 为前缀；按 key 或说明理解其含义，需要时再取值，不必一次性全部读取。\n\n")
		b.WriteString("| 环境变量名 | key | 说明 | 是否敏感 |\n")
		b.WriteString("|-----------|-----|------|---------|\n")
		for _, r := range rows {
			secret := "否"
			if r.IsSecret {
				secret = "是"
			}
			note := strings.ReplaceAll(r.Note, "|", "\\|")
			b.WriteString("| `$" + KeyToEnvName(r.Key) + "` | `" + r.Key + "` | " + note + " | " + secret + " |\n")
		}
		template += b.String()
	}

	return os.WriteFile(filepath.Join(workDir, "CLAUDE.md"), []byte(template), 0644)
}

// BuildTaskDir 创建 workdir + 写入 CLAUDE.md
func BuildTaskDir(workDir string, s *store.Store, rawCfg map[string]interface{}, dataDir string) (*string, error) {
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir workdir: %w", err)
	}

	// 仅在目录中没有 CLAUDE.md 时写入（resume 场景）
	claudeMdPath := filepath.Join(workDir, "CLAUDE.md")
	if _, err := os.Stat(claudeMdPath); os.IsNotExist(err) {
		if err := BuildClaudeMd(workDir, s, rawCfg, dataDir); err != nil {
			return nil, fmt.Errorf("write CLAUDE.md: %w", err)
		}
	}
	return &claudeMdPath, nil
}

// SessionWorkDir 返回某个 session 的工作目录。
// 根目录为 runtime/tasks（不落在 data/ 挂载卷内），子目录按 sessionID 命名，
// 保证同一 linkid 复用 session 时目录稳定，且不会因 linkid 变化丢失上下文。
func SessionWorkDir(runtimeTasksDir, sessionID string) string {
	return filepath.Join(runtimeTasksDir, SanitizeDirName(sessionID))
}

// SanitizeDirName 清理目录名中的非法字符（与 linkid 目录名清理逻辑一致）。
func SanitizeDirName(s string) string {
	r := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_",
	)
	return r.Replace(s)
}

func loadFile(relativePath string) string {
	candidates := []string{
		filepath.Join("prompts", relativePath),
		filepath.Join("..", "prompts", relativePath),
	}
	for _, p := range candidates {
		if data, err := os.ReadFile(p); err == nil {
			return string(data)
		}
	}
	return ""
}

func loadAllFiles() map[string]string {
	files := make(map[string]string)
	candidates := []string{"prompts", "../prompts"}
	for _, dir := range candidates {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		walkDir(dir, "", files)
		break
	}
	return files
}

func walkDir(dir, prefix string, files map[string]string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		full := filepath.Join(dir, entry.Name())
		rel := prefix + entry.Name()
		if entry.IsDir() {
			walkDir(full, rel+"/", files)
		} else if strings.HasSuffix(entry.Name(), ".md") {
			if data, err := os.ReadFile(full); err == nil {
				files[rel] = string(data)
			}
		}
	}
}

func defaultClaudeMd() string {
	return `# AI 数字员工 工作区

你是 AI 数字员工，运行在这个临时工作目录中。你需要自主完成任务，包括调用外部接口、下载文件、解析文档等。

## 文件下载
- 用 curl 下载文件到当前工作目录
- 文件 URL 已开放白名单，无需认证

## 行为规范
1. 严格按提示词要求的 JSON Schema 输出
2. 最终结果用 ` + "```json" + ` 代码块包裹
3. 不要输出任何其他内容
`
}
