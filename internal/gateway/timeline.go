package gateway

import (
	"encoding/json"
	"fmt"
	"strings"

	"dp-agt-dsh/internal/store"
)

// ─────────────────────────────────────────────────────────────────────
// 会话处理时间线 — 按 linkid 聚合所有 task（仅保留 Agent 输出）
// ─────────────────────────────────────────────────────────────────────

type historyTaskView struct {
	TaskID       string          `json:"task_id"`
	SessionID    string          `json:"session_id"`
	Title        string          `json:"title,omitempty"`
	Path         string          `json:"path"`
	Status       string          `json:"status"`
	ErrorMessage string          `json:"error_message"`
	CreatedAt    string          `json:"created_at"`
	FinishedAt   string          `json:"finished_at"`
	Events       []timelineEvent `json:"events"`
}

// buildOutputOnlyEvents 只保留每个任务的「Agent 输出结果」
func buildOutputOnlyEvents(task *store.TaskLog) []timelineEvent {
	if task.SdkOutput == "" {
		return []timelineEvent{}
	}
	return []timelineEvent{{
		Time:   task.FinishedAt,
		Type:   "output",
		Title:  "Agent 输出结果",
		Detail: task.SdkOutput,
	}}
}

// tryExtractCallbackFromTrace 从 SDK trace 中提取回调 curl 记录
func tryExtractCallbackFromTrace(sdkTrace, fallbackTime string) *timelineEvent {
	if sdkTrace == "" {
		return nil
	}
	var messages []map[string]interface{}
	if err := json.Unmarshal([]byte(sdkTrace), &messages); err != nil {
		return nil
	}
	if len(messages) == 0 {
		return nil
	}

	callbackIds := make(map[string]bool)
	for _, msg := range messages {
		if getStr(msg, "type") != "assistant" {
			continue
		}
		msgObj, _ := msg["message"].(map[string]interface{})
		content, _ := msgObj["content"].([]interface{})
		for _, blockRaw := range content {
			block, _ := blockRaw.(map[string]interface{})
			if getStr(block, "type") != "tool_use" || getStr(block, "name") != "Bash" {
				continue
			}
			input, _ := block["input"].(map[string]interface{})
			cmd := getStr(input, "command")
			if strings.Contains(cmd, "/tmp/callback_payload") || strings.Contains(cmd, "callback") {
				if id := getStr(block, "id"); id != "" {
					callbackIds[id] = true
				}
			}
		}
	}
	if len(callbackIds) == 0 {
		return nil
	}

	var results []string
	for _, msg := range messages {
		if getStr(msg, "type") != "user" {
			continue
		}
		msgObj, _ := msg["message"].(map[string]interface{})
		content, _ := msgObj["content"].([]interface{})
		for _, blockRaw := range content {
			block, _ := blockRaw.(map[string]interface{})
			if getStr(block, "type") != "tool_result" {
				continue
			}
			toolID := getStr(block, "tool_use_id")
			if toolID == "" || !callbackIds[toolID] {
				continue
			}
			switch raw := block["content"].(type) {
			case string:
				results = append(results, raw)
			case []interface{}:
				var sb strings.Builder
				for _, c := range raw {
					if cm, ok := c.(map[string]interface{}); ok {
						sb.WriteString(getStr(cm, "text"))
					}
				}
				results = append(results, sb.String())
			}
		}
	}
	if len(results) == 0 {
		return nil
	}

	allOk := true
	for _, r := range results {
		if strings.Contains(r, "curl: (") || strings.Contains(r, "command failed") || strings.Contains(r, "Error") {
			allOk = false
			break
		}
	}

	var detail string
	if len(results) > 1 {
		var parts []string
		for i, r := range results {
			parts = append(parts, fmt.Sprintf("[%d]\n%s", i+1, strings.TrimSpace(r)))
		}
		detail = strings.Join(parts, "\n\n")
	} else {
		detail = strings.TrimSpace(results[0])
	}

	title := "回调失败"
	typ := "error"
	if allOk {
		title = "回调成功"
		typ = "done"
	}
	return &timelineEvent{Time: fallbackTime, Type: typ, Title: title, Detail: detail}
}

// renderHistoryHTML 渲染按 linkid 聚合的会话处理时间线 HTML（折叠式，每任务一个 details/summary）
func renderHistoryHTML(linkid string, tasks []historyTaskView) string {
	var taskBlocks []string
	for idx, t := range tasks {
		endStr := t.FinishedAt
		if endStr == "" {
			endStr = "处理中"
		}
		statusCls := "info"
		if t.Status == "completed" {
			statusCls = "done"
		} else if t.Status == "failed" {
			statusCls = "error"
		}

		head := fmt.Sprintf(`<strong>%d.</strong>
		<span class="time">%s</span>
		<span>%s</span>
		<span style="font-family:monospace;font-size:0.78rem;">Task ID: %s</span>`, idx+1, escHTML(endStr), escHTML(statusLabel(t.Status)), escHTML(t.TaskID))

		// 会话处理时间线只保留「Agent 输出结果」
		var outputDetail string
		for _, ev := range t.Events {
			if ev.Type == "output" && ev.Detail != "" {
				outputDetail = ev.Detail
				break
			}
		}
		if len(outputDetail) == 0 && len(t.Events) > 0 && t.Events[0].Detail != "" {
			outputDetail = t.Events[0].Detail
		}

		if outputDetail == "" {
			taskBlocks = append(taskBlocks, fmt.Sprintf(`<div class="task-group">
		<div class="task-group-head %s">%s</div>
	</div>`, statusCls, head))
		} else {
			taskBlocks = append(taskBlocks, fmt.Sprintf(`<details class="task-group">
		<summary class="task-group-head %s">%s</summary>
		<div class="tl"><pre class="detail">%s</pre></div>
	</details>`, statusCls, head, escHTML(outputDetail)))
		}
	}

	firstStart := "-"
	if len(tasks) > 0 {
		firstStart = tasks[0].CreatedAt
	}
	lastEnd := "处理中"
	if len(tasks) > 0 && tasks[len(tasks)-1].FinishedAt != "" {
		lastEnd = tasks[len(tasks)-1].FinishedAt
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>会话处理时间线 — %s</title>
<style>%s
.task-group{border:1px solid #e0e3e8;border-radius:10px;margin-bottom:24px;overflow:hidden}
.task-group-head{display:flex;flex-wrap:wrap;align-items:center;gap:12px;padding:12px 20px;background:#fafbfc;border-bottom:1px solid #edf2f7;font-size:0.85rem;cursor:pointer}
.task-group-head.done{border-left:4px solid #2e7d32}
.task-group-head.error{border-left:4px solid #e53e3e}
.task-group-head.info{border-left:4px solid #4b6cb7}
details.task-group > summary{list-style:none}
details.task-group > summary::-webkit-details-marker{display:none}
details.task-group[open] > summary{border-bottom:1px solid #edf2f7}
.task-group .tl{padding:12px 24px}
</style>
</head>
<body>
<div class="wrap">
  <div class="page-head">
    <h1>&#129302; 会话处理时间线</h1>
    <div class="meta">
      <div class="meta-line"><span class="meta-label">Link ID:</span> %s</div>
      <div class="meta-line"><span class="meta-label">处理次数:</span> %d</div>
      <div class="meta-line"><span class="meta-label">时间范围:</span> %s ~ %s</div>
    </div>
  </div>
  <div style="padding:28px 36px;">%s</div>
  <div class="foot">Powered by AI 数字员工</div>
</div>
</body>
</html>`, escHTML(linkid), timelineCSS, escHTML(linkid), len(tasks), escHTML(firstStart), escHTML(lastEnd), strings.Join(taskBlocks, "\n"))
}

// renderHistoryMd 渲染按 linkid 聚合的会话处理时间线 Markdown
func renderHistoryMd(linkid string, tasks []historyTaskView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 会话处理时间线 — %s\n\n", linkid)
	fmt.Fprintf(&b, "- Link ID: %s\n", linkid)
	fmt.Fprintf(&b, "- 处理次数: %d\n\n", len(tasks))

	for idx, t := range tasks {
		endStr := t.FinishedAt
		if endStr == "" {
			endStr = "处理中"
		}
		fmt.Fprintf(&b, "## %d. %s %s Task ID: %s\n\n", idx+1, endStr, statusLabel(t.Status), t.TaskID)
		for _, ev := range t.Events {
			if ev.Detail != "" {
				fence := strings.Repeat("`", maxBacktickRun(ev.Detail)+1)
				if len(fence) < 3 {
					fence = "```"
				}
				b.WriteString(fence + "markdown\n")
				b.WriteString(ev.Detail + "\n")
				b.WriteString(fence + "\n")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func maxBacktickRun(s string) int {
	max := 0
	cur := 0
	for _, ch := range s {
		if ch == '`' {
			cur++
			if cur > max {
				max = cur
			}
		} else {
			cur = 0
		}
	}
	return max
}

func getStr(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
