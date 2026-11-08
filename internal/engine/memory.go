package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// MemoryItem 记忆库文件条目
type MemoryItem struct {
	Filename    string `json:"filename"`    // 文件名，如 xxx.md
	Title       string `json:"title"`       // 从文件第一行 # 提取
	Description string `json:"description"` // 标题后第一个非空段落，作为一句话说明
	Size        int64  `json:"size"`        // 文件字节数
}

// MemoryDir 返回记忆库目录
func MemoryDir(dataDir string) string {
	return filepath.Join(dataDir, "memory")
}

// EnsureMemoryDir 确保记忆库目录存在
func EnsureMemoryDir(dataDir string) string {
	dir := MemoryDir(dataDir)
	_ = os.MkdirAll(dir, 0755)
	return dir
}

// parseMemory 解析单个 .md 文件的标题与说明
func parseMemory(filePath, filename string) MemoryItem {
	title := strings.TrimSuffix(filename, ".md")
	description := ""
	var size int64
	if st, err := os.Stat(filePath); err == nil {
		size = st.Size()
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return MemoryItem{Filename: filename, Title: title, Description: description, Size: size}
	}
	lines := strings.Split(string(data), "\n")
	reTitle := regexp.MustCompile(`^#\s+(.+)`)
	for _, line := range lines {
		if m := reTitle.FindStringSubmatch(line); m != nil {
			title = strings.TrimSpace(m[1])
			break
		}
	}
	// 说明：标题行之后第一个非空、非纯标题段落
	afterTitle := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if afterTitle && trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			description = strings.NewReplacer(">", "", "-", "", "*", "", "|", "", "#", "", "`", "").Replace(trimmed)
			description = strings.TrimSpace(description)
			break
		}
		if strings.HasPrefix(trimmed, "#") {
			afterTitle = true
		}
	}
	return MemoryItem{Filename: filename, Title: title, Description: description, Size: size}
}

// ListMemories 列出所有记忆文件
func ListMemories(dataDir string) []MemoryItem {
	dir := MemoryDir(dataDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var items []MemoryItem
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		items = append(items, parseMemory(filepath.Join(dir, entry.Name()), entry.Name()))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Filename < items[j].Filename })
	return items
}

// GetMemory 读取单个记忆文件内容
func GetMemory(dataDir, filename string) (string, bool) {
	safe := sanitizeMemoryFilename(filename)
	if safe == "" {
		return "", false
	}
	full := filepath.Join(MemoryDir(dataDir), safe)
	data, err := os.ReadFile(full)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// SaveMemory 写入记忆文件（同名覆盖），返回安全文件名
func SaveMemory(dataDir, filename, content string) (string, error) {
	EnsureMemoryDir(dataDir)
	safe := sanitizeMemoryFilename(filename)
	if safe == "" {
		return "", os.ErrInvalid
	}
	full := filepath.Join(MemoryDir(dataDir), safe)
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		return "", err
	}
	return safe, nil
}

// DeleteMemory 删除记忆文件
func DeleteMemory(dataDir, filename string) bool {
	safe := sanitizeMemoryFilename(filename)
	if safe == "" {
		return false
	}
	full := filepath.Join(MemoryDir(dataDir), safe)
	if err := os.Remove(full); err != nil {
		return false
	}
	return true
}

// sanitizeMemoryFilename 仅保留 basename，拒绝路径穿越，且必须以 .md 结尾
func sanitizeMemoryFilename(filename string) string {
	base := filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	if base == "." || base == ".." || !strings.HasSuffix(base, ".md") {
		return ""
	}
	return base
}
