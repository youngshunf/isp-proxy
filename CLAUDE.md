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
| `web/` | pnpm 工作区：`apps/portal` 用户前台原型、`apps/admin` 管理后台原型（React 19 + antd 6 + TanStack Query + React Router Data 模式，内存模拟数据；后台趋势图用按需加载的 ECharts），`packages/api-client` 契约类型、管理端 `AdminApi`（fetch 客户端 + `createMockAdminApi`）与用户端 mock，`packages/ui` 两端共用的 token、主题、格式化与部件。视觉规范见 `DESIGN.md`；转正步骤见 `docs/05-施工文档.md` §6 |
| `internal/agent/` | ISP 平台节点代理原型：按订阅计量（`meter.go`）、连接表与 GOST 组件注册、失败即拒绝的出口链（`gost.go`）、订阅状态与持久化（`agent.go`、`store.go`）。设计见 `docs/04-计量与执行方案.md`；尚未接入 `cmd/gost` |

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

`internal/agent/` has in-process tests on loopback only (fake ISP upstream, no external network): `go test ./internal/agent/` (~3s; add `-race` for concurrency changes). They rely on GOST internals listed in docs/04 §2 — rerun them after any `go-gost/x` upgrade.

`web/` tests: `cd web && corepack pnpm@10.28.1 -r typecheck && corepack pnpm@10.28.1 -r test` (Vitest + Testing Library; mock API only). Dev servers: `corepack pnpm@10.28.1 dev:portal` → http://127.0.0.1:5173, `corepack pnpm@10.28.1 dev:admin` → http://127.0.0.1:5174 (mock login `ops@example.com` / `admin-pass-1`, TOTP `246810`). Build: `corepack pnpm@10.28.1 -r build`. Installing packages needs network access.
