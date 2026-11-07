# dp-agt-dsh — AI 数字员工（DSH SDK 网关）

接收 JSON → 按 path 查找提示词模板 → 渲染提示词 → 异步调用 DSH SDK → 回调结果。
系统本身不做业务逻辑，所有「智能」行为由 DSH SDK 的 tool use 完成。

## 敏感信息脱敏（必须遵守，最高优先级）

本仓库涉及真实的单位、人员和内部系统信息，**任何新增或修改的代码、提示词、示例数据、日志、注释中，都严禁出现真实信息**。

- **单位名称**：一律用占位符（如 `某公司`、`xx公司`、`某外部单位`、`某分公司-某部门`），不得写真实单位名、集团名、品牌名。
- **人名**：一律用占位符（如 `张三`、`李四`、`某处理人`），不得写真实姓名。
- **账号 / 域名前缀**：示例账号用虚构值，如 `exampleUser`；域名前缀用 `ptr` 之外的通用占位，不得暴露真实域前缀或 LDAP/AD 域名。
- **内部系统标识**：网关名、模型名、接口地址等内部命名（如 `api-gate`、`deepseek-v4-pro`、`oa-server:8080`）仅出现在配置或必要的内部说明中，示例和对外文档里用 `{{占位符}}` 或 `example.com` 代替。
- **Token / 密钥 / 密码**：只允许通过环境变量或 `prompt_config` 表（加密）引用，示例中只写 `$FILE_URL_TOKEN`、`$CALLBACK_TOKEN` 等变量名，绝不写死真实值。
- **日志输出**：打印配置、请求体时先脱敏（参考 `internal/store/store.go` 的 `maskValue()`、`internal/gateway/admin.go` 的 `maskIfPresent()`），请求体、文档内容等可能含真实单位/人名的内容，落到日志前要确认或脱敏。

已存在仓库中的示例数据（`张三`、`某分公司-某部门`、`北京XX科技有限公司` 等）均已是脱敏占位符，修改时保持脱敏，不要替换成真实值。

## 技术栈

- Go（`go.mod` 模块 `dp-agt-dsh`，`go 1.24`）
- 依赖：`mattn/go-sqlite3`（CGO 驱动）、`golang.org/x/crypto`（bcrypt/aes-gcm）、`gopkg.in/yaml.v3`
- 服务始终 HTTPS（证书缺失时本地自动生成自签名证书，或由 `build/build.sh` 生成到 `deploy/certs/`）
- 前端为 Vanilla JS SPA（无构建步骤）

## 项目结构

- `cmd/dp-agt-dsh/main.go`：服务入口（路由注册、TLS 启动、定时清理、一次性迁移）
- `internal/config/`：读取 `data/cfg.yml`（server/sys/llm），支持环境变量覆盖
- `internal/store/`：SQLite 持久化（`agent_interfaces`、`task_logs`、`sessions`、`users`、`prompt_config` 加密 KV、`config`）、加解密、用户认证
- `internal/engine/`：提示词加载/渲染（`prompt.go`）、异步任务队列（`queue.go`）、业务记忆库（`memory.go`）、思维导图（`xmind.go`）
- `internal/harness/`：DSH SDK 子进程客户端
- `internal/gateway/`：HTTP 处理器（`process.go` 对外 API、`admin.go` 管理后台、`auth*.go` 认证、`timeline.go` 时间线）
- `prompts/`：提示词模板，目录结构即对外访问路径
  - `prompts/doc/*.md` → 访问路径 `doc/*`；`claude-md.md`、`sections/` 不作为访问路径
- `web/`：管理后台前端（Vanilla JS SPA，`login.html`、`admin.html`）
- `data/`：运行时数据（SQLite `app.db` + 任务工作目录 + 记忆库，含 `secrets.key`、`cfg.yml`/`cfg.csv`，勿提交）
- `bin/`：DSH 运行时二进制（由 `build/fetch-dsh-runtime.sh` 从 PyPI 拉取，不进 Git）
- `build/`、`deploy/`：Docker 构建与部署（`deploy/certs/` 为自动生成证书，不进 Git）

## 开发约定

- 构建：`go build -o dp-agt-dsh ./cmd/dp-agt-dsh`；Docker 镜像构建：`./build/build.sh`。
- 无 `npm`/热更新脚本，本地调试直接 `go build` + 运行二进制。
- 提示词占位符语法：业务模板用 `{{request.xxx}}` 与 `{{task_id}}`；CLAUDE.md 模板（`prompts/claude-md.md`）用 `{{key}}`（来自 CSV 配置），条件段落用 `{{section:片段名:key}}`。
- `prompts/` 下的 `.md` 在**启动时**和**上传提示词压缩包时**被扫描进内存缓存；直接改文件后需重启服务或通过管理后台重新上传提示词包才会生效（无文件监听热更新）。
- 任务工作目录按 linkid 创建于 `data/tasks/<linkid>/`；CLAUDE.md 只在目录中不存在时写入（resume 场景不重写），session 复用的任务不会重新读取 CLAUDE.md。
- 所有面向业务的文本（`fail_reason`、`opinion_text`、`result_text` 等）要求简体中文。

## 配置说明

- `data/cfg.yml`：服务端口、并发数、LLM 地址/模型等（模板见 `data/cfg.yml.template`）。
- `data/cfg.csv`：业务 KV 配置，导入后存 `prompt_config` 表（值 AES-GCM 加密），供 CLAUDE.md 模板 `{{key}}` 渲染（模板见 `data/cfg.csv.template`）。
- 密钥类值只通过环境变量（如 `DEEPSEEK_API_KEY`、`TLS_CERT_FILE`/`TLS_KEY_FILE`）或加密 `prompt_config` 引用，不写死。

## Git 注意

- 不要提交 `data/` 下的运行时数据、`secrets.key`、证书（`deploy/certs/`）、`bin/` 下 DSH 运行时二进制、`node_modules`、构建产物（见 `.gitignore`）。
- 提交/推送前确认改动未引入真实单位名、人名、Token 等敏感信息。
