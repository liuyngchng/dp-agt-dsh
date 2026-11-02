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

// BuildClaudeMd 构建 CLAUDE.md 并把它写入工作目录
func BuildClaudeMd(workDir string, s *store.Store, rawCfg map[string]interface{}) error {
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

	// {{section:name:trigger_key}}
	reSection := regexp.MustCompile(`\{\{section:(\w+):([\w.]+)\}\}`)
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

	// {{key}} (通用)
	reKey := regexp.MustCompile(`\{\{([\w.]+)\}\}`)
	template = reKey.ReplaceAllStringFunc(template, func(match string) string {
		key := reKey.FindStringSubmatch(match)[1]
		if strings.HasPrefix(key, "section:") || strings.HasPrefix(key, "request.") {
			return match
		}
		if val, ok := cfgVals[key]; ok {
			return val
		}
		return match
	})

	// 把模板里残留的环境变量引用转为实际值（用于提示词中的 $FILE_URL_TOKEN 等）
	for k, v := range cfgVals {
		if strings.HasPrefix(k, "doc.") && strings.Contains(template, "$"+strings.ToUpper(strings.ReplaceAll(k, ".", "_"))) {
			// 已通过 {{key}} 替换
		}
		_ = v
	}

	return os.WriteFile(filepath.Join(workDir, "CLAUDE.md"), []byte(template), 0644)
}

// BuildTaskDir 创建 workdir + 写入 CLAUDE.md
func BuildTaskDir(workDir string, s *store.Store, rawCfg map[string]interface{}) (*string, error) {
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir workdir: %w", err)
	}

	// 仅在目录中没有 CLAUDE.md 时写入（resume 场景）
	claudeMdPath := filepath.Join(workDir, "CLAUDE.md")
	if _, err := os.Stat(claudeMdPath); os.IsNotExist(err) {
		if err := BuildClaudeMd(workDir, s, rawCfg); err != nil {
			return nil, fmt.Errorf("write CLAUDE.md: %w", err)
		}
	}
	return &claudeMdPath, nil
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