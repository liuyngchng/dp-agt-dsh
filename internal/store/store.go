package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

// Store SQLite 持久化存储
type Store struct {
	db *sql.DB
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir data: %w", err)
	}

	dbPath := filepath.Join(dataDir, "app.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) migrate() error {
	ddl := []string{
		`CREATE TABLE IF NOT EXISTS agent_interfaces (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL,
			path        TEXT NOT NULL UNIQUE,
			prompt      TEXT NOT NULL DEFAULT '',
			created_at  TEXT,
			updated_at  TEXT,
			created_ts  INTEGER,
			updated_ts  INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS prompt_config (
			key         TEXT PRIMARY KEY,
			value_enc   TEXT NOT NULL DEFAULT '',
			iv          TEXT NOT NULL DEFAULT '',
			tag         TEXT NOT NULL DEFAULT '',
			is_secret   INTEGER NOT NULL DEFAULT 0,
			note        TEXT DEFAULT '',
			updated_at  TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS task_logs (
			task_id          TEXT PRIMARY KEY,
			linkid           TEXT,
			session_id       TEXT,
			path             TEXT,
			interface_id     TEXT,
			status           TEXT,
			request_body     TEXT,
			request_response TEXT,
			prompt           TEXT,
			sdk_output       TEXT,
			sdk_trace        TEXT,
			error_message    TEXT,
			created_at       TEXT,
			finished_at      TEXT,
			created_ts       INTEGER,
			finished_ts      INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			session_id  TEXT PRIMARY KEY,
			linkid      TEXT NOT NULL UNIQUE,
			status      TEXT NOT NULL DEFAULT 'active',
			created_at  TEXT,
			updated_at  TEXT,
			created_ts  INTEGER,
			updated_ts  INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			username        TEXT UNIQUE NOT NULL,
			password_hash   TEXT NOT NULL,
			role            INTEGER DEFAULT 0,
			note            TEXT DEFAULT '',
			must_change_pwd INTEGER DEFAULT 0,
			user_version    INTEGER DEFAULT 1,
			created_at      TEXT,
			created_ts      INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS config (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			note  TEXT
		)`,
	}
	for _, d := range ddl {
		if _, err := s.db.Exec(d); err != nil {
			return fmt.Errorf("ddl: %w", err)
		}
	}

	// 兼容旧库：补列
	_, _ = s.db.Exec(`ALTER TABLE users ADD COLUMN user_version INTEGER DEFAULT 1`)

	// 种子 admin
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		count = 0
	}
	if count == 0 {
		hash, _ := HashPassword("admin")
		now := time.Now().UnixMilli()
		_, _ = s.db.Exec(
			"INSERT INTO users (username, password_hash, role, note, must_change_pwd, user_version, created_at, created_ts) VALUES (?, ?, ?, ?, ?, 1, ?, ?)",
			"admin", hash, 1, "默认管理员", 1, timeFmt(now), now,
		)
		log.Println("[store] seeded default admin user (admin/admin)")
	}
	return nil
}

// ── AgentInterface ──

type AgentInterface struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Prompt    string `json:"prompt"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	CreatedTs int64  `json:"created_ts"`
	UpdatedTs int64  `json:"updated_ts"`
}

func (s *Store) GetInterfaceByPath(path string) (*AgentInterface, error) {
	row := s.db.QueryRow(`SELECT id, name, path, prompt,
		COALESCE(created_at,''), COALESCE(updated_at,''), COALESCE(created_ts,0), COALESCE(updated_ts,0)
		FROM agent_interfaces WHERE path = ?`, path)
	var iface AgentInterface
	err := row.Scan(&iface.ID, &iface.Name, &iface.Path, &iface.Prompt,
		&iface.CreatedAt, &iface.UpdatedAt, &iface.CreatedTs, &iface.UpdatedTs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &iface, nil
}

func (s *Store) UpsertInterface(id, name, p, prompt string) error {
	now := time.Now().UnixMilli()
	ts := timeFmt(now)
	_, err := s.db.Exec(
		`INSERT INTO agent_interfaces (id, name, path, prompt, created_at, updated_at, created_ts, updated_ts)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET name=excluded.name, prompt=excluded.prompt,
		 updated_at=excluded.updated_at, updated_ts=excluded.updated_ts`,
		id, name, p, prompt, ts, ts, now, now,
	)
	return err
}

func (s *Store) ListInterfaces() ([]AgentInterface, error) {
	rows, err := s.db.Query(`SELECT id, name, path, prompt,
		COALESCE(created_at,''), COALESCE(updated_at,''), COALESCE(created_ts,0), COALESCE(updated_ts,0)
		FROM agent_interfaces ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentInterface
	for rows.Next() {
		var iface AgentInterface
		if err := rows.Scan(&iface.ID, &iface.Name, &iface.Path, &iface.Prompt,
			&iface.CreatedAt, &iface.UpdatedAt, &iface.CreatedTs, &iface.UpdatedTs); err != nil {
			return nil, err
		}
		out = append(out, iface)
	}
	return out, nil
}

// ── TaskLog ──

type TaskStatus string

const (
	StatusPending    TaskStatus = "pending"
	StatusProcessing TaskStatus = "processing"
	StatusFailed     TaskStatus = "failed"
	StatusCompleted  TaskStatus = "completed"
	StatusCancelled  TaskStatus = "cancelled"
)

type TaskLog struct {
	TaskID          string     `json:"task_id"`
	LinkID          string     `json:"linkid"`
	SessionID       string     `json:"session_id"`
	Path            string     `json:"path"`
	InterfaceID     string     `json:"interface_id"`
	Status          TaskStatus `json:"status"`
	RequestBody     string     `json:"request_body"`
	RequestResponse string     `json:"request_response"`
	Prompt          string     `json:"prompt"`
	SdkOutput       string     `json:"sdk_output"`
	SdkTrace        string     `json:"sdk_trace"`
	ErrorMessage    string     `json:"error_message"`
	CreatedAt       string     `json:"created_at"`
	FinishedAt      string     `json:"finished_at"`
	CreatedTs       int64      `json:"created_ts"`
	FinishedTs      int64      `json:"finished_ts"`
}

func (s *Store) CreateTaskLog(t *TaskLog) error {
	_, err := s.db.Exec(
		`INSERT INTO task_logs (task_id, linkid, session_id, path, interface_id, status,
		 request_body, request_response, prompt, sdk_output, sdk_trace,
		 error_message,
		 created_at, finished_at, created_ts, finished_ts)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.TaskID, t.LinkID, t.SessionID, t.Path, t.InterfaceID, t.Status,
		t.RequestBody, t.RequestResponse, t.Prompt, t.SdkOutput, t.SdkTrace,
		t.ErrorMessage,
		t.CreatedAt, t.FinishedAt, t.CreatedTs, t.FinishedTs,
	)
	return err
}

func (s *Store) GetTaskLog(taskID string) (*TaskLog, error) {
	row := s.db.QueryRow(`SELECT task_id, COALESCE(linkid,''), COALESCE(session_id,''),
		COALESCE(path,''), COALESCE(interface_id,''), COALESCE(status,''),
		COALESCE(request_body,''), COALESCE(request_response,''), COALESCE(prompt,''),
		COALESCE(sdk_output,''), COALESCE(sdk_trace,''), COALESCE(error_message,''),
		COALESCE(created_at,''), COALESCE(finished_at,''), COALESCE(created_ts,0), COALESCE(finished_ts,0)
		FROM task_logs WHERE task_id = ?`, taskID)
	var t TaskLog
	err := row.Scan(&t.TaskID, &t.LinkID, &t.SessionID, &t.Path, &t.InterfaceID, &t.Status,
		&t.RequestBody, &t.RequestResponse, &t.Prompt, &t.SdkOutput, &t.SdkTrace,
		&t.ErrorMessage,
		&t.CreatedAt, &t.FinishedAt, &t.CreatedTs, &t.FinishedTs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) GetLatestTaskByLinkid(linkID string) (*TaskLog, error) {
	return s.getTaskByWhere("linkid = ? ORDER BY created_at DESC LIMIT 1", linkID)
}

func (s *Store) getTaskByWhere(where string, args ...interface{}) (*TaskLog, error) {
	row := s.db.QueryRow(`SELECT task_id, COALESCE(linkid,''), COALESCE(session_id,''),
		COALESCE(path,''), COALESCE(interface_id,''), COALESCE(status,''),
		COALESCE(request_body,''), COALESCE(request_response,''), COALESCE(prompt,''),
		COALESCE(sdk_output,''), COALESCE(sdk_trace,''), COALESCE(error_message,''),
		COALESCE(created_at,''), COALESCE(finished_at,''), COALESCE(created_ts,0), COALESCE(finished_ts,0)
		FROM task_logs WHERE `+where, args...)
	var t TaskLog
	err := row.Scan(&t.TaskID, &t.LinkID, &t.SessionID, &t.Path, &t.InterfaceID, &t.Status,
		&t.RequestBody, &t.RequestResponse, &t.Prompt, &t.SdkOutput, &t.SdkTrace,
		&t.ErrorMessage,
		&t.CreatedAt, &t.FinishedAt, &t.CreatedTs, &t.FinishedTs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) UpdateTaskResult(taskID string, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
	}
	query := "UPDATE task_logs SET "
	var vals []interface{}
	first := true
	for k, v := range fields {
		if !first {
			query += ", "
		}
		query += k + " = ?"
		vals = append(vals, v)
		first = false
	}
	query += " WHERE task_id = ?"
	vals = append(vals, taskID)
	_, err := s.db.Exec(query, vals...)
	return err
}

func (s *Store) HasActiveByLinkid(linkID string) bool {
	var count int
	s.db.QueryRow("SELECT COUNT(*) FROM task_logs WHERE linkid = ? AND status IN ('pending', 'processing')", linkID).Scan(&count)
	return count > 0
}

func (s *Store) CountActiveTasks() int {
	var count int
	s.db.QueryRow("SELECT COUNT(*) FROM task_logs WHERE status IN ('pending', 'processing')").Scan(&count)
	return count
}

func (s *Store) ListActiveTasks() ([]TaskLog, error) {
	return s.queryTasks("WHERE status IN ('pending', 'processing') ORDER BY created_at ASC")
}

func (s *Store) ListTasksPaged(taskID, linkID string, offset, limit int) ([]TaskLog, int, error) {
	where := ""
	var args []interface{}
	if taskID != "" {
		where = "WHERE task_id LIKE ?"
		args = append(args, "%"+taskID+"%")
	}
	if linkID != "" {
		if where == "" {
			where = "WHERE linkid LIKE ?"
		} else {
			where += " AND linkid LIKE ?"
		}
		args = append(args, "%"+linkID+"%")
	}

	var total int
	countQ := "SELECT COUNT(*) FROM task_logs " + where
	s.db.QueryRow(countQ, args...).Scan(&total)

	dataQ := "SELECT task_id, COALESCE(linkid,''), COALESCE(session_id,''), COALESCE(path,''), COALESCE(interface_id,''), COALESCE(status,''), COALESCE(request_body,''), COALESCE(request_response,''), COALESCE(prompt,''), COALESCE(sdk_output,''), COALESCE(sdk_trace,''), COALESCE(error_message,''), COALESCE(created_at,''), COALESCE(finished_at,''), COALESCE(created_ts,0), COALESCE(finished_ts,0) FROM task_logs " + where + " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	dataArgs := append(append([]interface{}{}, args...), limit, offset)
	rows, err := s.db.Query(dataQ, dataArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	tasks, err := scanTasks(rows)
	return tasks, total, err
}

func (s *Store) queryTasks(where string, args ...interface{}) ([]TaskLog, error) {
	rows, err := s.db.Query(`SELECT task_id, COALESCE(linkid,''), COALESCE(session_id,''),
		COALESCE(path,''), COALESCE(interface_id,''), COALESCE(status,''),
		COALESCE(request_body,''), COALESCE(request_response,''), COALESCE(prompt,''),
		COALESCE(sdk_output,''), COALESCE(sdk_trace,''), COALESCE(error_message,''),
		COALESCE(created_at,''), COALESCE(finished_at,''), COALESCE(created_ts,0), COALESCE(finished_ts,0)
		FROM task_logs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

func scanTasks(rows *sql.Rows) ([]TaskLog, error) {
	var out []TaskLog
	for rows.Next() {
		var t TaskLog
		if err := rows.Scan(&t.TaskID, &t.LinkID, &t.SessionID, &t.Path, &t.InterfaceID, &t.Status,
			&t.RequestBody, &t.RequestResponse, &t.Prompt, &t.SdkOutput, &t.SdkTrace,
			&t.ErrorMessage,
			&t.CreatedAt, &t.FinishedAt, &t.CreatedTs, &t.FinishedTs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// ── Session ──

type Session struct {
	SessionID string `json:"session_id"`
	LinkID    string `json:"linkid"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	CreatedTs int64  `json:"created_ts"`
	UpdatedTs int64  `json:"updated_ts"`
}

func (s *Store) GetSessionByLinkid(linkID string) (*Session, error) {
	row := s.db.QueryRow(`SELECT session_id, linkid, status,
		COALESCE(created_at,''), COALESCE(updated_at,''), COALESCE(created_ts,0), COALESCE(updated_ts,0)
		FROM sessions WHERE linkid = ? AND status = 'active'`, linkID)
	var sess Session
	err := row.Scan(&sess.SessionID, &sess.LinkID, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt, &sess.CreatedTs, &sess.UpdatedTs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &sess, err
}

func (s *Store) UpsertSession(sessionID, linkID string) error {
	now := time.Now().UnixMilli()
	ts := timeFmt(now)
	_, err := s.db.Exec(
		`INSERT INTO sessions (session_id, linkid, status, created_at, updated_at, created_ts, updated_ts)
		 VALUES (?, ?, 'active', ?, ?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET updated_at=excluded.updated_at, updated_ts=excluded.updated_ts`,
		sessionID, linkID, ts, ts, now, now,
	)
	return err
}

func (s *Store) ListAllSessions() ([]Session, error) {
	rows, err := s.db.Query(`SELECT session_id, linkid, status,
		COALESCE(created_at,''), COALESCE(updated_at,''), COALESCE(created_ts,0), COALESCE(updated_ts,0)
		FROM sessions ORDER BY updated_ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.SessionID, &sess.LinkID, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt, &sess.CreatedTs, &sess.UpdatedTs); err != nil {
			continue
		}
		out = append(out, sess)
	}
	return out, nil
}

func (s *Store) GetSession(sessionID string) (*Session, error) {
	row := s.db.QueryRow(`SELECT session_id, linkid, status,
		COALESCE(created_at,''), COALESCE(updated_at,''), COALESCE(created_ts,0), COALESCE(updated_ts,0)
		FROM sessions WHERE session_id = ?`, sessionID)
	var sess Session
	err := row.Scan(&sess.SessionID, &sess.LinkID, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt, &sess.CreatedTs, &sess.UpdatedTs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &sess, err
}

func (s *Store) RemoveSession(sessionID string) bool {
	result, err := s.db.Exec("DELETE FROM sessions WHERE session_id = ?", sessionID)
	if err != nil {
		return false
	}
	n, _ := result.RowsAffected()
	return n > 0
}

// ── PromptConfig（加密 KV） ──

var secretKey []byte

func InitCrypto(dataDir string) error {
	keyFile := filepath.Join(dataDir, "secrets.key")
	key, err := os.ReadFile(keyFile)
	if err != nil || len(key) != 32 {
		key = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return fmt.Errorf("rand: %w", err)
		}
		if err := os.WriteFile(keyFile, key, 0600); err != nil {
			return fmt.Errorf("write secrets.key: %w", err)
		}
	}
	secretKey = key
	return nil
}

func encryptValue(plain string) (enc, iv, tag string) {
	if secretKey == nil || plain == "" {
		return plain, "", ""
	}
	block, err := aes.NewCipher(secretKey)
	if err != nil {
		return plain, "", ""
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return plain, "", ""
	}
	nonce := make([]byte, aesgcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return plain, "", ""
	}
	ciphertext := aesgcm.Seal(nil, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), base64.StdEncoding.EncodeToString(nonce), ""
}

func decryptValue(enc, iv, tag string) string {
	if secretKey == nil || enc == "" {
		return enc
	}
	_ = tag
	block, err := aes.NewCipher(secretKey)
	if err != nil {
		return enc
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return enc
	}
	ciphertext, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return enc
	}
	nonce, err := base64.StdEncoding.DecodeString(iv)
	if err != nil {
		return enc
	}
	plain, err := aesgcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return enc
	}
	return string(plain)
}

type ConfigRow struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	IsSecret  bool   `json:"is_secret"`
	Note      string `json:"note"`
	UpdatedAt string `json:"updated_at"`
}

func (s *Store) ImportConfigRows(rows []ConfigRow) error {
	now := timeFmt(time.Now().UnixMilli())
	for _, row := range rows {
		enc, iv, tag := encryptValue(row.Value)
		isSecret := 0
		if row.IsSecret {
			isSecret = 1
		}
		_, err := s.db.Exec(
			`INSERT INTO prompt_config (key, value_enc, iv, tag, is_secret, note, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(key) DO UPDATE SET value_enc=excluded.value_enc, iv=excluded.iv,
			 tag=excluded.tag, is_secret=excluded.is_secret, note=excluded.note, updated_at=excluded.updated_at`,
			row.Key, enc, iv, tag, isSecret, row.Note, now,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetConfigPlain(key string) (string, bool) {
	var enc, iv, tag string
	err := s.db.QueryRow("SELECT value_enc, iv, tag FROM prompt_config WHERE key = ?", key).Scan(&enc, &iv, &tag)
	if err != nil {
		return "", false
	}
	return decryptValue(enc, iv, tag), true
}

func (s *Store) GetAllConfigPlain() map[string]string {
	out := make(map[string]string)
	rows, err := s.db.Query("SELECT key, value_enc, iv, tag FROM prompt_config")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, enc, iv, tag string
		if err := rows.Scan(&k, &enc, &iv, &tag); err != nil {
			continue
		}
		if val := decryptValue(enc, iv, tag); val != "" {
			out[k] = val
		}
	}
	return out
}

func (s *Store) ListConfigRows(reveal bool) []ConfigRow {
	rows, err := s.db.Query("SELECT key, value_enc, iv, tag, is_secret, COALESCE(note,''), COALESCE(updated_at,'') FROM prompt_config ORDER BY key")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ConfigRow
	for rows.Next() {
		var k, enc, iv, tag, note, updatedAt string
		var isSecret int
		if err := rows.Scan(&k, &enc, &iv, &tag, &isSecret, &note, &updatedAt); err != nil {
			continue
		}
		plain := decryptValue(enc, iv, tag)
		if isSecret == 1 && !reveal {
			plain = maskValue(plain)
		}
		out = append(out, ConfigRow{Key: k, Value: plain, IsSecret: isSecret == 1, Note: note, UpdatedAt: updatedAt})
	}
	return out
}

func (s *Store) ListConfigKeys() []string {
	rows, err := s.db.Query("SELECT key FROM prompt_config ORDER BY key")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		rows.Scan(&k)
		out = append(out, k)
	}
	return out
}

func (s *Store) AllConfigKeysSet() map[string]bool {
	set := make(map[string]bool)
	for _, k := range s.ListConfigKeys() {
		set[k] = true
	}
	return set
}

func (s *Store) ClearConfigRows() error {
	_, err := s.db.Exec("DELETE FROM prompt_config")
	return err
}

// ── Config KV ──

func (s *Store) GetConfig(key string) string {
	var val string
	s.db.QueryRow("SELECT value FROM config WHERE key = ?", key).Scan(&val)
	return val
}

func (s *Store) SetConfig(key, value, note string) error {
	_, err := s.db.Exec("INSERT INTO config (key, value, note) VALUES (?, ?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, note=excluded.note", key, value, note)
	return err
}

// ── Users ──

type User struct {
	ID            int    `json:"id"`
	Username      string `json:"username"`
	PasswordHash  string `json:"-"`
	Role          int    `json:"role"`
	Note          string `json:"note"`
	MustChangePwd bool   `json:"must_change_pwd"`
	UserVersion   int    `json:"user_version"`
	CreatedAt     string `json:"created_at"`
	CreatedTs     int64  `json:"created_ts"`
}

func (s *Store) GetUser(username string) (*User, error) {
	row := s.db.QueryRow("SELECT id, username, password_hash, role, COALESCE(note,''), COALESCE(must_change_pwd,0), COALESCE(user_version,1), COALESCE(created_at,''), COALESCE(created_ts,0) FROM users WHERE username = ?", username)
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Note, &u.MustChangePwd, &u.UserVersion, &u.CreatedAt, &u.CreatedTs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &u, err
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query("SELECT id, username, password_hash, role, COALESCE(note,''), COALESCE(must_change_pwd,0), COALESCE(user_version,1), COALESCE(created_at,''), COALESCE(created_ts,0) FROM users ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Note, &u.MustChangePwd, &u.UserVersion, &u.CreatedAt, &u.CreatedTs); err != nil {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

func (s *Store) CreateUser(username, hash string, role int, note string) (bool, error) {
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(
		"INSERT INTO users (username, password_hash, role, note, must_change_pwd, user_version, created_at, created_ts) VALUES (?, ?, ?, ?, 0, 1, ?, ?)",
		username, hash, role, note, timeFmt(now), now,
	)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) DeleteUser(username string) bool {
	result, err := s.db.Exec("DELETE FROM users WHERE username = ?", username)
	if err != nil {
		return false
	}
	n, _ := result.RowsAffected()
	return n > 0
}

func (s *Store) UpdatePassword(username, hash string) bool {
	result, err := s.db.Exec("UPDATE users SET password_hash = ?, user_version = user_version + 1 WHERE username = ?", hash, username)
	if err != nil {
		return false
	}
	n, _ := result.RowsAffected()
	return n > 0
}

func (s *Store) ClearMustChangePwd(username string) bool {
	result, err := s.db.Exec("UPDATE users SET must_change_pwd = 0 WHERE username = ?", username)
	if err != nil {
		return false
	}
	n, _ := result.RowsAffected()
	return n > 0
}

func (s *Store) IncrementUserVersion(username string) {
	_, _ = s.db.Exec("UPDATE users SET user_version = user_version + 1 WHERE username = ?", username)
}

// ── Cleanup ──

func (s *Store) CleanupExpired(retentionDays int) (deletedTasks int, expiredSessions []Session) {
	cutoffMs := time.Now().UnixMilli() - int64(retentionDays*24*3600*1000)

	// 过期任务
	result, err := s.db.Exec("DELETE FROM task_logs WHERE created_ts < ? AND status NOT IN ('pending', 'processing')", cutoffMs)
	if err == nil {
		n, _ := result.RowsAffected()
		deletedTasks = int(n)
	}

	// 读过期 session（删除前先读出来方便外部清理磁盘）
	rows, err := s.db.Query(`SELECT session_id, linkid, status,
		COALESCE(created_at,''), COALESCE(updated_at,''), COALESCE(created_ts,0), COALESCE(updated_ts,0)
		FROM sessions WHERE updated_ts < ?`, cutoffMs)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var sess Session
			if err := rows.Scan(&sess.SessionID, &sess.LinkID, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt, &sess.CreatedTs, &sess.UpdatedTs); err != nil {
				continue
			}
			expiredSessions = append(expiredSessions, sess)
		}
	}
	for _, sess := range expiredSessions {
		s.db.Exec("DELETE FROM sessions WHERE session_id = ?", sess.SessionID)
	}

	return
}

func (s *Store) ActiveSessionLinkIDs() map[string]bool {
	rows, err := s.db.Query("SELECT linkid FROM sessions WHERE status = 'active'")
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var id string
		rows.Scan(&id)
		out[id] = true
	}
	return out
}

// ── 辅助 ──

func timeFmt(ms int64) string {
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05.000")
}

func maskValue(plain string) string {
	if len(plain) <= 4 {
		return "****"
	}
	return plain[:2] + "****" + plain[len(plain)-2:]
}

func HashPassword(pw string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

func TimeNowFmt() string {
	return timeFmt(time.Now().UnixMilli())
}

func TimeNowMs() int64 {
	return time.Now().UnixMilli()
}
