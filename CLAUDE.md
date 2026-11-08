# CLAUDE.md — 驿卡 · Yika（isp-proxy）

驿卡是基于 GOST v3 fork 的 ISP 代理销售平台：把第三方 ISP 上游转成按订阅计量的 HTTP / HTTPS / SOCKS5 服务，含控制面 `isp-control`（Go + PostgreSQL）、带 agent 的 `gost` 节点、用户前台与管理后台（React）。

产品方向、阶段与当前状态见 `PROJECT.md`。按任务读：
- 业务规则（订阅、流量、停用、换接入/换出口、决策 D-xx）：`docs/01-需求文档.md`
- 验收场景 A-xx 与对应测试：`docs/02-需求核对与验收清单.md`
- 计量、断连、GOST 接缝：`docs/04-计量与执行方案.md`
- 工程约定（端口、迁移、接口、测试分层）：`docs/06-实施约定.md`
- 里程碑与开发栈：`docs/05-施工文档.md`
- 前端视觉：`DESIGN.md`、`PRODUCT.md`

## 提交与合入（硬性规则）

本机主 clone `/Users/mac/www/isp-proxy`（分支 `main`）是唯一的合并点和推送出口。主 clone 的 `main` 始终与 `origin/main` 一致。

1. 开工前：在主 clone 执行 `git pull --ff-only`，让它与远端一致。
2. 开发：功能在独立分支或 worktree 里开发、提交。
3. 合并：测试通过后，在主 clone 里把分支合进 `main`。
4. 推送：从主 clone 执行 `git push origin main`。
5. 收尾：确认主 clone 显示 `main...origin/main` 没有 ahead/behind。

不要从分支或 worktree 直接推送远端 `main`（例如 `git push origin HEAD:main`）：这样主 clone 会落后远端。

如果会话被隔离在 worktree 里、无法操作主 clone，先退出 worktree，再在主 clone 完成第 3～5 步。

测试通过后直接合并推送，不用先问主人；测试失败不合。

## 约束

- 文档、注释用中文；提交信息格式 `<type>: <中文描述>`。
- 真实 ISP 凭据、客户连接、支付密钥、数据库、日志、服务器地址与密码都不进 Git，示例一律用占位值。服务器配置与部署信息文档放在 `deploy/remote/targets/`（已忽略）。
- 接真实 ISP、真实邮件收发、真实收款的测试，要主人单独授权。本机回环的平台 E2E 与浏览器 E2E 不需要授权。
- 未实现或未实测的能力，不写成已交付。

## 目录

| 路径 | 内容 |
| --- | --- |
| `cmd/isp-control/` | 控制面入口：`serve`（启动即迁移；对外 API 默认 `:8080`，节点 mTLS 默认 `:8443`）、`migrate`、`dev-certs`、`admin create`、`mail-test`（按配置直发测试邮件）、`seed-dev`；YAML 配置 + 环境变量密钥（docs/06 §2） |
| `cmd/gost/` | GOST 入口；`-agent-config <file>` 以 ISP 节点模式启动（拒绝 `-api`/`GOST_API`/`-R`；不带该参数时行为与上游一致） |
| `cmd/isp-devstack/` | 本机开发栈：一条命令起一次性 PostgreSQL、`isp-control`（文件投递邮件）、seed-dev、管理员（打印 TOTP 密钥）、假 ISP 上游与回显端点、`gost -agent-config`；就绪信息写 `<dir>/state.json`（浏览器 E2E 读它） |
| `internal/agent/` | 节点数据面：按订阅计量（`meter.go`）、连接表与失败即拒绝的出口链（`gost.go`）、订阅状态与持久化、脱敏日志器 `isp-redact` |
| `internal/agent/node/` | 节点运行时：mTLS 连控制面对账、上报、探测，落盘 `desired.json`（0600，含凭据）。该模式禁止重载：重载会清空 `isp-auth` 与出口链，导致直连（docs/04 §2） |
| `internal/agent/agenttest/` | 测试辅助：伪控制面、伪 ISP 上游（http/https/socks5）、三种协议客户端 |
| `internal/agentproto/` | 控制面 ↔ agent 协议与开发证书生成 |
| `internal/control/core/` | 业务内核组装：`kernel`、`ledger`、`inventory`（共享出口名额 `tenancy.go`）、`catalog`、`probe`、`task`、`alert`、`subscription`、`order`、`exitreq`、`agentsrv`、`overview`；5 秒调度器；验收用例 A-xx 在 `core/*_test.go` |
| `internal/control/api/` | 路由组装与统一鉴权（CSRF 双提交、会话、封禁只读、viewer 只读）；契约测试断言路由集合 = `api/openapi.yaml` |
| `internal/control/httpx/` | HTTP 公共约定：错误码映射、严格 `DecodeJSON`、`Route`、`ClientIP` |
| `internal/control/{account,admin}/` | 用户邮箱账号（注册、验证、登录、找回）；管理员密码 + TOTP、用户管理、审计 |
| `internal/control/{audit,mail,session,ratelimit,secret}/` | 审计；邮件发件箱 + SMTP/文件投递；Cookie 与 CSRF；限流；AES-GCM/MAC/argon2id |
| `migrations/` | 只前进的 SQL 迁移，内嵌并在启动时执行 |
| `api/openapi.yaml` | 对外 API 契约，前端类型由它生成 |
| `web/` | pnpm 工作区：`apps/portal` 用户前台、`apps/admin` 管理后台（React 19 + antd 6 + TanStack Query + React Router Data 模式）；`packages/api-client` 契约类型、fetch 客户端与 mock；`packages/ui` 共用 token、主题与部件；`e2e` Playwright 浏览器用例 |
| `tests/isp/` | 平台 E2E（build tag `ispe2e`），每个 docs/02 场景一个测试 |
| `deploy/` | 单节点部署示例（systemd、nginx、`backup.sh`、配置示例、`seed.example.sql`），全部为占位值 |
| `deploy/remote/` | 一键部署：`deploy/remote/yika.sh <目标> deploy`，其余子命令见 `deploy/remote/README.md` |
| `tests/e2e/`、其余顶层目录 | 上游 GOST 原有代码与测试 |

## 命令

```bash
# Go
go build ./... && go vet ./...
ISP_TEST_REQUIRE_DB=1 ISP_REQUIRE_COMPLETE_API=1 go test -race ./internal/... ./cmd/... ./deploy/...
make isp-e2e        # 平台 E2E，约 1 分钟，需要本机 initdb/pg_ctl/pg_dump/pg_restore

# 前端（在 web/ 下；装包需要联网）
corepack pnpm@10.28.1 -r typecheck && corepack pnpm@10.28.1 -r test && corepack pnpm@10.28.1 -r build
corepack pnpm@10.28.1 gen          # 在 web/packages/api-client 下，改了 openapi.yaml 后重新生成类型
corepack pnpm@10.28.1 e2e          # 浏览器 E2E，自起开发栈 + Vite，约 3 分钟

# 本机开发
go run ./cmd/isp-devstack          # 控制面 :8080（-listen 可改，改了要给 Vite 设 ISP_CONTROL_URL）
corepack pnpm@10.28.1 dev:portal   # http://127.0.0.1:5173
corepack pnpm@10.28.1 dev:admin    # http://127.0.0.1:5174
```

- **测试数据库**：DB 测试用 `pgtest` 起一次性 PostgreSQL（本机 `initdb`/`pg_ctl`，或 `ISP_TEST_DATABASE_URL`）。没有数据库时这些测试会跳过；设 `ISP_TEST_REQUIRE_DB=1` 时改为失败。
- **ISSUE 标记**：标 `ISSUE:` 的失败表示产品缺陷。
- **前端 mock**：`VITE_PORTAL_API=mock` / `VITE_ADMIN_API=mock` 切到内存 mock。mock 管理员是 `ops@example.com` / `admin-pass-1`，TOTP 固定为 `246810`。
- **Vite 模块缓存**：开发服务器长时间运行、期间合并过 `web/packages/*` 的改动后，页面若报“does not provide an export named …”，说明模块缓存旧了，用 `--force` 重启。

## GOST 内核（fork）

- 内置组件靠 `init()` 注册，`cmd/gost/register.go` 用空导入触发。新增 handler、listener、dialer、connector 时要在这里加一行。
- `internal/agent/` 与 `go test ./cmd/gost/` 依赖 docs/04 §2 列出的 GOST 内部行为，升级 `go-gost/x` 后必须重跑这两组测试。
- 远端 `upstream` 指向 `go-gost/gost`，只用来读取和合并上游。合并前核对 docs/04 §2 的接缝。来源与基线见 `UPSTREAM.toml`。
- `tests/e2e/` 是上游的二进制 E2E，只有主人明确要求时才运行。
