package harness

// DSH SDK JSON-RPC 协议类型定义。
// 协议: newline-delimited JSON-RPC 2.0 over stdio

// ── JSON-RPC 基础帧 ──

type JsonRpcRequest struct {
	Jsonrpc string      `json:"jsonrpc"`
	ID      string      `json:"id,omitempty"`
	Method  string      `json:"method,omitempty"`
	Params  interface{} `json:"params,omitempty"`
}

type JsonRpcResponse struct {
	Jsonrpc string        `json:"jsonrpc"`
	ID      string        `json:"id,omitempty"`
	Result  interface{}   `json:"result,omitempty"`
	Error   *JsonRpcError `json:"error,omitempty"`
}

type JsonRpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// ── Notification ──

type Notification struct {
	Method  string                 `json:"method"`
	Payload map[string]interface{} `json:"params"`
}

// ── initialize ──

type InitializeParams struct {
	Cwd              string                 `json:"cwd"`
	Provider         string                 `json:"provider"`
	Model            string                 `json:"model"`
	ReasoningEffort  string                 `json:"reasoningEffort,omitempty"`
	MaxTokens        int                    `json:"maxTokens,omitempty"`
	Capabilities     map[string]interface{} `json:"capabilities,omitempty"`
	ApprovalRequests bool                   `json:"approvalRequests,omitempty"`
}

type InitializeResult struct {
	ServerInfo ServerInfo `json:"serverInfo"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ── session/prompt ──

type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type SessionPromptParams struct {
	SessionID     string         `json:"sessionId"`
	ContentBlocks []ContentBlock `json:"contentBlocks"`
}

// SessionPromptResult 是 dsh 0.2.1 协议的 session/prompt 返回：
// 只有 messageId（0.2.2 的 session/query 才返回 kind: prompt/command）。
type SessionPromptResult struct {
	MessageID string `json:"messageId"`
}

// ── session/resume ──

type SessionResumeParams struct {
	SessionID string `json:"sessionId"`
}

// ── session/new ──

type SessionNewParams struct {
	Cwd string `json:"cwd"`
}

// ── session/working-directory/set ──

type SessionWorkingDirectorySetParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
}

type SessionWorkingDirectoryResult struct {
	Cwd string `json:"cwd"`
}

// ── shutdown ──

type ShutdownResult struct{}

// ── Run result ──

type RunResult struct {
	SessionID     string         `json:"session_id"`
	FinalResponse string         `json:"final_response"`
	FinishReason  string         `json:"finish_reason"`
	Events        []SessionEvent `json:"events"`
}

// ── Session event ──

type SessionEvent struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data"`
	Time int64                  `json:"time"` // dsh 事件外层 time（毫秒时间戳）
}

// FinalResponse 从 events 中提取最后一个 assistant/message 的文本
func FinalResponse(events []SessionEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != "assistant/message" {
			continue
		}
		msg, ok := events[i].Data["message"].(map[string]interface{})
		if !ok {
			msg = events[i].Data
		}
		content, ok := msg["content"].([]interface{})
		if !ok {
			continue
		}
		var parts []string
		for _, block := range content {
			b, ok := block.(map[string]interface{})
			if !ok {
				continue
			}
			if t, _ := b["type"].(string); t == "text" {
				if txt, _ := b["text"].(string); txt != "" {
					parts = append(parts, txt)
				}
			}
		}
		return joinStrings(parts)
	}
	return ""
}

// FinishReason 从 events 中提取最后一个 turn/end 的 reason.kind
func FinishReason(events []SessionEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != "turn/end" {
			continue
		}
		reason, ok := events[i].Data["reason"].(map[string]interface{})
		if !ok {
			continue
		}
		if kind, ok := reason["kind"].(string); ok {
			return kind
		}
	}
	return ""
}

func joinStrings(parts []string) string {
	var result string
	for _, p := range parts {
		result += p
	}
	return result
}
