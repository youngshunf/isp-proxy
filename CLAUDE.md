# CLAUDE.md — gost/

CLI binary entry point for GOST (GO Simple Tunnel). This module compiles the `gost` command.

## Build & Run

```bash
cd gost && go build ./cmd/gost/...

# Cross-compile
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" ./cmd/gost/...

# Build all platforms
make all

# Run
./gost -L "http://:8080" -F "socks5://:1080"
./gost -C gost.yml
```

## Structure

| File | Purpose |
|------|---------|
| `cmd/gost/main.go` | CLI entry point: flag parsing, multi-worker mode (`--` separator), program launch via `svc.Run` |
| `cmd/gost/program.go` | Service lifecycle (`Init`/`Start`/`Stop`), config loading, API/metrics/profiling servers, SIGHUP reload |
| `cmd/gost/register.go` | Blank imports for all built-in handlers, listeners, dialers, connectors — triggers `init()` registration |
| `cmd/gost/version.go` | Version resolution: ldflags injection → `buildVersionFallback` + short VCS commit → module build info → bare `buildVersionFallback` |
| `web/` | pnpm 工作区：`apps/portal` 用户前台原型（React 19 + antd 6 + TanStack Query + React Router Data 模式，内存模拟数据），`packages/api-client` 契约类型与 mock。视觉规范见 `DESIGN.md`；转正步骤见 `docs/05-施工文档.md` §6 |
| `internal/agent/` | ISP 平台节点 agent 数据面：按订阅计量（`meter.go`）、连接表与 GOST 组件注册、失败即拒绝的出口链（`gost.go`）、订阅状态与持久化（`agent.go`、`store.go`）。设计见 `docs/04-计量与执行方案.md` |
| `internal/agent/node/` | 节点运行时：读 agent 配置（`deploy/agent.example.yaml`），mTLS 连控制面按期望状态对账、上报、探测，落盘最后已应用状态（`desired.json`，0600，含凭据）。由 `gost -agent-config <file>` 启动；该模式不开放 GOST 原生 API、禁止重载（重载会清空 `isp-auth` 与出口链并导致直连，见 docs/04 §2） |
| `internal/agent/agenttest/` | 测试辅助：伪控制面（`agentproto.NewHandler` + 开发证书）、伪 ISP 上游（http/https/socks5）、三种协议客户端 |
| `cmd/isp-control/` | 控制面入口：`serve`（启动即迁移，`:8080` 对外 API，`:8443` 节点 mTLS）、`migrate`、`dev-certs`、`admin create`、`seed-dev`；YAML 配置 + 环境变量密钥（docs/06 §2） |
| `internal/control/httpx/` | HTTP 公共约定：`*httpx.Error` 与错误码映射、严格 `DecodeJSON`、`Route{Method, Pattern, Auth}`、`UserFrom`/`AdminFrom`、`ClientIP` |
| `internal/control/api/` | 组装全部业务包路由（`Assemble`，ctl-core 接入点在 `wiring.go`），统一执行 CSRF 双提交、会话校验、`VerifiedUser`/封禁只读/viewer 只读；契约测试断言路由集合 = `apispec`（暂缺清单 `notYetImplemented`） |
| `internal/control/account/` | 用户邮箱账号（C-W10）：注册、验证、重发、登录、找回、改密、会话校验；不泄露邮箱是否注册；库内限流 |
| `internal/control/admin/` | 管理员（C-W8）：密码 + TOTP 两步登录（首次绑定、恢复码）、管理员会话、用户管理、审计查询、`admin.Create`（CLI） |
| `internal/control/{audit,mail,session,ratelimit,secret}/` | 审计写入（`audit.Record(ctx, tx, …)`）与查询；邮件发件箱 + SMTP/文件投递；Cookie 与 CSRF；`rate_events` 滑动窗口；AES-GCM/MAC/argon2id |
| `deploy/` | 单节点部署示例（systemd、nginx、`backup.sh`、配置与环境变量示例、`seed.example.sql`），全部为占位值 |

## CLI Flags

| Flag | Usage |
|------|-------|
| `-L` | Inline service definition (repeatable) |
| `-F` | Inline chain node definition (repeatable) |
| `-C` | Path to config file (YAML/JSON) |
| `-D` / `-DD` | Debug / trace logging |
| `-api` | API service address (e.g. `:8080`) |
| `-metrics` | Prometheus metrics address |
| `-O` | Output merged config as yaml or json, then exit |
| `-V` | Print version and exit |
| `-agent-config` | ISP node agent config file (YAML); absent = upstream GOST behavior. Refuses `-api`/`GOST_API`/`-R` |

## Multi-Worker Mode

Arguments separated by ` -- ` spawn multiple worker processes via `exec.CommandContext`. Each worker runs as a child process with `_GOST_ID` set. Any worker's exit cancels all others. This is triggered in `init()` before flag parsing.

## Service Lifecycle

- `Init` → calls `parser.Init()` with all CLI/config inputs
- `Start` → `parser.Parse()` → `loader.Load()` → `p.run()` (starts services, API, metrics, profiling)
- `Stop` → cancels reload context, closes all services
- SIGHUP → `reloadConfig()` re-parses and re-runs without restarting the process

## Key Dependencies

- `github.com/go-gost/core` — interface definitions
- `github.com/go-gost/x` — all implementations, config, registry
- `github.com/judwhite/go-svc` — OS service framework (handles daemon/SIGHUP/SIGTERM)

## Registration Pattern

All components register via `init()` side-effects in their packages. `cmd/gost/register.go` triggers them with blank imports. When adding a new built-in handler/listener/dialer/connector, add a blank import here.

## Tests

E2E tests are in `tests/e2e/` — integration tests using the built binary. Run with `go test ./tests/e2e/...` only when explicitly authorized (see PROJECT.md).

`internal/agent/` has in-process tests on loopback only (fake ISP upstream, no external network): `go test ./internal/agent/...` (~20s incl. node runtime; add `-race` for concurrency changes). `go test ./cmd/gost/` builds the real binary and runs it with `-agent-config` against a fake control plane (loopback only; `-short` skips the binary tests). They rely on GOST internals listed in docs/04 §2 — rerun them after any `go-gost/x` upgrade.

Control plane: `go test ./internal/control/... ./cmd/isp-control/...` — DB tests use a throwaway PostgreSQL from `pgtest` (local `initdb`/`pg_ctl`, or `ISP_TEST_DATABASE_URL`; skipped with a reason if neither exists, set `ISP_TEST_REQUIRE_DB=1` to fail instead). HTTP tests run against the real router (`internal/control/api`); mail goes to a file-drop dir, never real SMTP.

`web/` tests: `cd web && corepack pnpm@10.28.1 -r typecheck && corepack pnpm@10.28.1 -r test` (Vitest + Testing Library; mock API only). Dev server: `corepack pnpm@10.28.1 dev:portal` → http://127.0.0.1:5173. Installing packages needs network access.
