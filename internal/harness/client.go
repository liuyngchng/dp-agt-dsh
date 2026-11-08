package harness

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"dp-agt-dsh/internal/config"
)

// ── 错误 ──

var (
	ErrTransportClosed   = errors.New("dsh runtime is not running")
	ErrProtocolViolation = errors.New("protocol violation")
)

type RpcError struct {
	Code    int
	Message string
}

func (e *RpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// ── 通知订阅 ──

type notificationSub struct {
	ch     chan Notification
	filter func(Notification) bool
}

// ── Client ──

type Client struct {
	cfg config.LLMConfig

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	stderr io.ReadCloser

	mu      sync.Mutex
	pending map[string]chan *json.RawMessage
	nextID  atomic.Int64

	// pending 独立加锁：readLoop 投递响应时不与 Start/request 的生命周期锁 c.mu 竞争，
	// 否则 Start 持有 c.mu 等待 initialize 响应时，readLoop 拿不到 c.mu 会死锁超时。
	pendingMu sync.Mutex

	subMu   sync.Mutex
	subs    map[int64]*notificationSub
	nextSub atomic.Int64

	stderrLines []string

	initialized bool
	closed      atomic.Bool

	model    string
	provider string
	baseURL  string
	apiKey   string
	dsHome   string

	// runtimeDir 是 dsh 子进程的进程级工作目录（initialize.cwd 与 cmd.Dir）。
	// 它必须存在且可读写，作为 session 创建后再切换到真实会话目录之前的过渡目录。
	runtimeDir string
}

// NewClient 创建 dsh harness 客户端（不启动进程）
func NewClient(cfg config.LLMConfig) *Client {
	return &Client{
		cfg:        cfg,
		pending:    make(map[string]chan *json.RawMessage),
		subs:       make(map[int64]*notificationSub),
		model:      cfg.Model,
		provider:   cfg.Provider,
		baseURL:    cfg.BaseURL,
		apiKey:     cfg.AuthToken,
		dsHome:     cfg.DSHome,
		runtimeDir: cfg.RuntimeDir,
	}
}

// Start 启动 dsh 子进程 + 初始化握手
func (c *Client) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cmd != nil {
		return nil
	}

	bin := findDSHBin()
	if bin == "" {
		return fmt.Errorf("dsh binary not found under bin/")
	}

	env := os.Environ()
	if c.baseURL != "" {
		env = append(env, "DEEPSEEK_BASE_URL="+c.baseURL)
	}
	if c.apiKey != "" {
		env = append(env, "DEEPSEEK_API_KEY="+c.apiKey)
	}
	if c.dsHome == "" {
		c.dsHome = filepath.Join(os.TempDir(), "dp-agt-dsh-home")
	}
	env = append(env, "DSH_HOME="+c.dsHome)
	// DSH SDK 默认沙箱模式为 workspace-write，依赖 bwrap/Landlock
	// OS 级文件隔离。本系统单租户运行，无多租户隔离需求，直接切到
	// danger-full-access，与 dp-agt 的 bypassPermissions 语义一致。
	env = append(env, "DSH_PERMISSION_MODE=danger-full-access")
	_ = os.MkdirAll(c.dsHome, 0755)

	runtimeDir := c.runtimeDir
	if runtimeDir == "" {
		runtimeDir = "/tmp"
	}
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		return fmt.Errorf("mkdir runtime dir: %w", err)
	}

	cmd := exec.Command(bin, "--profile", "sdk")
	cmd.Env = env
	cmd.Dir = runtimeDir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start dsh: %w", err)
	}

	c.cmd = cmd
	c.stdin = stdin
	c.stdout = bufio.NewScanner(stdout)
	c.stderr = stderr
	// dsh 事件行可能很长（含大段文本），给足缓冲
	c.stdout.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)

	go c.readLoop()
	go c.stderrLoop()

	// 初始化握手
	resp, err := c.requestLocked("initialize", InitializeParams{
		Cwd:      runtimeDir,
		Provider: c.provider,
		Model:    c.model,
	})
	if err != nil {
		c.killLocked()
		return fmt.Errorf("initialize: %w", err)
	}
	var initResult InitializeResult
	if err := json.Unmarshal(*resp, &initResult); err != nil {
		c.killLocked()
		return fmt.Errorf("parse initialize result: %w", err)
	}
	slog.Info("initialized", "server", initResult.ServerInfo.Name, "version", initResult.ServerInfo.Version, "model", c.model)
	c.initialized = true
	return nil
}

// Close 安全关闭 dsh 子进程
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cmd == nil {
		return nil
	}

	// 尝试正常 shutdown
	_, _ = c.requestLocked("shutdown", nil)

	if c.stdin != nil {
		_ = c.stdin.Close()
	}

	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
	}

	c.pendingMu.Lock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.pending = make(map[string]chan *json.RawMessage)
	c.pendingMu.Unlock()

	c.cmd = nil
	c.initialized = false
	return nil
}

// Run 在指定 session 上执行 prompt，返回 RunResult
func (c *Client) Run(sessionID string, prompt string, onNotify func(Notification)) (*RunResult, error) {
	// 必须先订阅再发送查询：dsh 会在 session/prompt 响应之前就发出
	// agent/inbox/spliced 事件（携带 messageId），后订阅会错过该事件，
	// 导致 receivedPrompt 永远为 false，任务卡死在 status 等待上。
	subID, subCh := c.subscribe(func(n Notification) bool {
		if sid, _ := n.Payload["sessionId"].(string); sid == sessionID {
			return true
		}
		// 子 agent 的父 session 关系暂不做精确过滤，交给上层
		return false
	})
	defer c.unsubscribe(subID)

	messageID, err := c.sendQuery(sessionID, prompt)
	if err != nil {
		return nil, err
	}

	var events []SessionEvent
	receivedPrompt := false

	for {
		note, ok := <-subCh
		if !ok {
			return nil, ErrTransportClosed
		}
		if onNotify != nil {
			onNotify(note)
		}

		if note.Method == "session.event" {
			sid, _ := note.Payload["sessionId"].(string)
			if sid == sessionID {
				if event, ok := note.Payload["event"].(map[string]interface{}); ok {
					// SessionEvent.Data 存的是事件真正的数据（event["data"]），
					// 而不是整个 event 外壳，否则会双重嵌套导致 FinalResponse 读不到 content。
					eventData, _ := event["data"].(map[string]interface{})
					se := SessionEvent{Type: getStr(event, "type"), Data: eventData, Time: getInt64(event, "time")}
					events = append(events, se)

					if !receivedPrompt && se.Type == "agent/inbox/spliced" {
						if data, ok := event["data"].(map[string]interface{}); ok {
							if inserted, ok := data["inserted"].([]interface{}); ok {
								for _, msg := range inserted {
									if m, ok := msg.(map[string]interface{}); ok && getStr(m, "id") == messageID {
										receivedPrompt = true
									}
								}
							}
						}
					}
				}
			}
		}

		if receivedPrompt && note.Method == "session.status" {
			if sid, _ := note.Payload["sessionId"].(string); sid == sessionID {
				if status, _ := note.Payload["status"].(string); status == "idle" {
					return &RunResult{
						SessionID:     sessionID,
						FinalResponse: FinalResponse(events),
						FinishReason:  FinishReason(events),
						Events:        events,
					}, nil
				}
			}
		}
	}
}

// ResumeSession 恢复持久化的 session
func (c *Client) ResumeSession(sessionID string) error {
	_, err := c.request("session/resume", SessionResumeParams{SessionID: sessionID})
	return err
}

// SetWorkingDirectory 设置 session 的工作目录
func (c *Client) SetWorkingDirectory(sessionID, workDir string) error {
	_, err := c.request("session/working-directory/set", SessionWorkingDirectorySetParams{
		SessionID: sessionID,
		Path:      workDir,
	})
	return err
}

// ── 请求 ──

func (c *Client) sendQuery(sessionID, promptText string) (string, error) {
	blocks := []ContentBlock{{Type: "text", Text: promptText}}
	raw, err := c.request("session/prompt", SessionPromptParams{
		SessionID:     sessionID,
		ContentBlocks: blocks,
	})
	if err != nil {
		return "", fmt.Errorf("session/prompt: %w", err)
	}
	var result SessionPromptResult
	if err := json.Unmarshal(*raw, &result); err != nil {
		return "", fmt.Errorf("parse session/prompt: %w", err)
	}
	if result.MessageID == "" {
		return "", fmt.Errorf("%w: empty messageId", ErrProtocolViolation)
	}
	return result.MessageID, nil
}

func (c *Client) request(method string, params interface{}) (*json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requestLocked(method, params)
}

func (c *Client) requestLocked(method string, params interface{}) (*json.RawMessage, error) {
	if c.cmd == nil || c.stdin == nil {
		return nil, ErrTransportClosed
	}

	id := fmt.Sprintf("%d", c.nextID.Add(1))
	ch := make(chan *json.RawMessage, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	req := JsonRpcRequest{Jsonrpc: "2.0", ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}

	if _, err := fmt.Fprintf(c.stdin, "%s\n", string(data)); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrTransportClosed, err)
	}

	select {
	case resp := <-ch:
		if resp == nil {
			return nil, ErrTransportClosed
		}
		var rpcResp JsonRpcResponse
		if err := json.Unmarshal(*resp, &rpcResp); err == nil && rpcResp.Error != nil {
			return nil, &RpcError{Code: rpcResp.Error.Code, Message: rpcResp.Error.Message}
		}
		return resp, nil
	case <-time.After(30 * time.Second):
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("request timeout: %s", method)
	}
}

// ── 订阅 ──

func (c *Client) subscribe(filter func(Notification) bool) (int64, chan Notification) {
	id := c.nextSub.Add(1)
	ch := make(chan Notification, 1024)
	c.subMu.Lock()
	c.subs[id] = &notificationSub{ch: ch, filter: filter}
	c.subMu.Unlock()
	return id, ch
}

func (c *Client) unsubscribe(id int64) {
	c.subMu.Lock()
	delete(c.subs, id)
	c.subMu.Unlock()
}

// ── 读循环 ──

func (c *Client) readLoop() {
	for c.stdout.Scan() {
		line := c.stdout.Text()
		if line == "" {
			continue
		}
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *JsonRpcError   `json:"error"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			continue
		}

		// 响应帧（有 id，无 method）
		if len(frame.ID) > 0 && frame.Method == "" {
			var idStr string
			_ = json.Unmarshal(frame.ID, &idStr)

			c.pendingMu.Lock()
			ch, ok := c.pending[idStr]
			if ok {
				if frame.Error != nil {
					// 构造错误响应
					errResp := JsonRpcResponse{Jsonrpc: "2.0", ID: idStr, Error: frame.Error}
					b, _ := json.Marshal(errResp)
					r := json.RawMessage(b)
					ch <- &r
				} else {
					r := frame.Result
					ch <- &r
				}
				delete(c.pending, idStr)
			}
			c.pendingMu.Unlock()
			continue
		}

		// 请求帧（有 id + method）：服务端请求，这里忽略
		if len(frame.ID) > 0 && frame.Method != "" {
			continue
		}

		// 通知帧（有 method，无 id）
		if frame.Method != "" {
			var params map[string]interface{}
			if len(frame.Params) > 0 {
				_ = json.Unmarshal(frame.Params, &params)
			}
			c.dispatchNotification(Notification{Method: frame.Method, Payload: params})
		}
	}

	// stdout 关闭 → 广播关闭
	c.subMu.Lock()
	for _, s := range c.subs {
		close(s.ch)
	}
	c.subs = make(map[int64]*notificationSub)
	c.subMu.Unlock()
}

func (c *Client) stderrLoop() {
	scanner := bufio.NewScanner(c.stderr)
	for scanner.Scan() {
		line := scanner.Text()
		c.mu.Lock()
		c.stderrLines = append(c.stderrLines, line)
		if len(c.stderrLines) > 200 {
			c.stderrLines = c.stderrLines[len(c.stderrLines)-200:]
		}
		c.mu.Unlock()
	}
}

func (c *Client) dispatchNotification(note Notification) {
	c.subMu.Lock()
	subs := make([]*notificationSub, 0, len(c.subs))
	for _, s := range c.subs {
		subs = append(subs, s)
	}
	c.subMu.Unlock()

	for _, s := range subs {
		if s.filter == nil || s.filter(note) {
			select {
			case s.ch <- note:
			default: // 订阅者消费慢，丢弃，避免阻塞读循环
			}
		}
	}
}

// ── 生命周期辅助 ──

func (c *Client) killLocked() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

// StderrTail 返回 stderr 尾部（诊断用）
func (c *Client) StderrTail() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.stderrLines) == 0 {
		return ""
	}
	out := ""
	for _, l := range c.stderrLines {
		out += l + "\n"
	}
	return out
}

// ── 辅助 ──

func findDSHBin() string {
	candidates := []string{
		"bin/deepseek-harness-sdk-runtime-linux-x64",
		"../bin/deepseek-harness-sdk-runtime-linux-x64",
		"./bin/deepseek-harness-sdk-runtime-linux-x64",
	}
	if exePath, err := os.Executable(); err == nil {
		dir := filepath.Dir(exePath)
		candidates = append(candidates,
			filepath.Join(dir, "bin", "deepseek-harness-sdk-runtime-linux-x64"),
			filepath.Join(dir, "..", "bin", "deepseek-harness-sdk-runtime-linux-x64"),
		)
	}
	for _, p := range candidates {
		if abs, err := filepath.Abs(p); err == nil {
			if st, err := os.Stat(abs); err == nil && !st.IsDir() {
				return abs
			}
		}
	}
	return ""
}

func getStr(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getInt64(m map[string]interface{}, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}
