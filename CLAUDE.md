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
| `web/` | pnpm 工作区：`apps/portal` 用户前台、`apps/admin` 管理后台（React 19 + antd 6 + TanStack Query + React Router Data 模式；默认连真实控制面，`VITE_PORTAL_API=mock` / `VITE_ADMIN_API=mock` 回到内存模拟；后台趋势图用按需加载的 ECharts），`packages/api-client` 契约类型、`PortalApi`（`/portal`）与 `AdminApi`（`/admin`）fetch 客户端及各自的 mock，`packages/ui` 两端共用的 token、主题、格式化与部件，`e2e` 浏览器端到端用例（Playwright）。视觉规范见 `DESIGN.md`；转正记录与开发栈用法见 `docs/05-施工文档.md` §6 |
| `internal/agent/` | ISP 平台节点 agent 数据面：按订阅计量（`meter.go`）、连接表与 GOST 组件注册、失败即拒绝的出口链（`gost.go`）、订阅状态与持久化（`agent.go`、`store.go`）、接入服务使用的脱敏日志器 `isp-redact`（`redact.go`，客户请求原文与认证明文不进 gost.log）。设计见 `docs/04-计量与执行方案.md` |
| `internal/agent/node/` | 节点运行时：读 agent 配置（`deploy/agent.example.yaml`），mTLS 连控制面按期望状态对账、上报、探测，落盘最后已应用状态（`desired.json`，0600，含凭据）。由 `gost -agent-config <file>` 启动；该模式不开放 GOST 原生 API、禁止重载（重载会清空 `isp-auth` 与出口链并导致直连，见 docs/04 §2） |
| `tests/isp/` | 平台 E2E（build tag `ispe2e`）：每个 docs/02 场景一个测试；环境 `env_test.go`、类型化客户端 `client_test.go`/`admin_test.go`/`user_test.go`、四种接入 `proxy_test.go`、业务流程 `flows_test.go`。见 docs/05 §7.1 |
| `internal/agent/agenttest/` | 测试辅助：伪控制面（`agentproto.NewHandler` + 开发证书）、伪 ISP 上游（http/https/socks5）、三种协议客户端 |
| `cmd/isp-control/` | 控制面入口：`serve`（启动即迁移，`:8080` 对外 API，`:8443` 节点 mTLS）、`migrate`、`dev-certs`、`admin create`、`seed-dev`（含独享与共享示例套餐）；YAML 配置 + 环境变量密钥（docs/06 §2）。`core.shared_exit_default_capacity`（默认 3）是录入上游不填绑定上限时的默认值（D-24） |
| `cmd/isp-devstack/` | 本机开发栈：一条命令起一次性 PostgreSQL、`isp-control`（文件投递邮件）、seed-dev、管理员（打印 TOTP 密钥）、假 ISP 上游与回显端点（agenttest）、`gost -agent-config`；打印地址与账号，Ctrl-C 清理；就绪信息写 `<dir>/state.json`（浏览器 E2E 读它）。见 docs/05 §6.1 |
| `internal/control/httpx/` | HTTP 公共约定：`*httpx.Error` 与错误码映射、严格 `DecodeJSON`、`Route{Method, Pattern, Auth}`、`UserFrom`/`AdminFrom`、`ClientIP` |
| `internal/control/api/` | 组装全部业务包路由（`Assemble`，ctl-core 接入点在 `wiring.go`），统一执行 CSRF 双提交、会话校验、`VerifiedUser`/封禁只读/viewer 只读；契约测试断言路由集合 = `apispec`（暂缺清单 `notYetImplemented` 已清空，`ISP_REQUIRE_COMPLETE_API=1` 强制） |
| `internal/control/core/` | 业务内核组装（`core.New(pool, kernel.Config, clock, mailer, auditor, logger)`）：`Domains()`/`Routes()` 路由组、`AgentServer()` 节点接口、`RunScheduler` 5 秒调度、`admin.Hooks` 三个回调；各业务包 `kernel`（环境、事务、`GeoLookup`）、`ledger`、`inventory`、`catalog`、`probe`、`task`、`alert`、`subscription`、`order`、`exitreq`、`agentsrv`、`overview`。共享出口（D-24）的名额模型在 `inventory/tenancy.go` 与迁移 `0005_shared_exits.sql`（docs/04 §7.6）。验收用例 A-xx 在 `core/*_test.go`（pgtest） |
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

Platform E2E: `make isp-e2e` (= `go test -tags ispe2e ./tests/isp/... -count=1`, ~45s, all parallel). One test per docs/02 scenario (`TestA05_…`); each runs its own throwaway PostgreSQL + `isp-control serve` + `gost -agent-config` + fake ISP exits, loopback only (no real ISP/mail/payment, so no extra authorization needed). Needs local `initdb`/`pg_ctl`/`pg_dump`/`pg_restore`. Failing tests that mark a product bug say `ISSUE:`. Control-plane dev knobs it uses: `core.scheduler_interval`/`task_timeout`/`probe_timeout`/`node_offline_after`, `dev.clock_offset_file` (business-clock offset; never in production). Direct proxy (D-25) config: `core.direct_default_bandwidth_mbps` (default 10, gift bandwidth default) and `core.direct_max_connections` (default 64). `ISP_E2E_KEEP=<dir>` keeps logs. See docs/05 §7.1.

`web/` tests: `cd web && corepack pnpm@10.28.1 -r typecheck && corepack pnpm@10.28.1 -r test` (Vitest + Testing Library; mock API only). Real backend for dev: `go run ./cmd/isp-devstack` (control plane on :8080; `-listen` to change, then set `ISP_CONTROL_URL` for Vite), then `corepack pnpm@10.28.1 dev:portal` → http://127.0.0.1:5173 and `corepack pnpm@10.28.1 dev:admin` → http://127.0.0.1:5174 (Vite proxies `/api` and `/admin-api`; ports via `PORTAL_PORT`/`ADMIN_PORT`). Mock-only: `VITE_PORTAL_API=mock` / `VITE_ADMIN_API=mock` (mock admin login `ops@example.com` / `admin-pass-1`, TOTP `246810`). Build: `corepack pnpm@10.28.1 -r build`. Browser E2E: `corepack pnpm@10.28.1 e2e` (Playwright on local Chrome; globalSetup starts its own devstack + Vite on free ports, ~3 min; docs/05 §7.2). Installing packages needs network access.
