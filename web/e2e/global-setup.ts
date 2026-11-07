// 启动开发栈（cmd/isp-devstack：一次性 PostgreSQL、isp-control、假 ISP 上游、gost agent）
// 与两个前端的 Vite 开发服务器，端口全部临时挑选，不碰已在运行的 5173/5174/8080。
// 设置 ISP_E2E_STATE=<已有开发栈的 state.json> 可以复用正在运行的开发栈（调试用例时省去启动时间）。
import { spawn, type ChildProcess } from 'node:child_process'
import { appendFileSync, existsSync, mkdirSync, openSync, readFileSync, rmSync } from 'node:fs'
import { createServer } from 'node:net'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const web = resolve(here, '..')
const repo = resolve(web, '..')

function freePort(): Promise<number> {
  return new Promise((ok, fail) => {
    const s = createServer()
    s.unref()
    s.on('error', fail)
    s.listen(0, '127.0.0.1', () => {
      const port = (s.address() as { port: number }).port
      s.close(() => ok(port))
    })
  })
}

async function waitFor(what: string, check: () => Promise<boolean> | boolean, timeoutMs: number, proc?: ChildProcess, log?: string) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (proc && (proc.exitCode !== null || proc.pid === undefined)) throw new Error(`${what}：进程已退出（${proc.exitCode}），见 ${log}`)
    try {
      if (await check()) return
    } catch {
      /* 还没起来 */
    }
    await new Promise((r) => setTimeout(r, 300))
  }
  throw new Error(`${what}：${timeoutMs / 1000}s 内未就绪，见 ${log}`)
}

function start(cmd: string, args: string[], cwd: string, env: Record<string, string>, log: string): ChildProcess {
  const fd = openSync(log, 'w')
  // 独立进程组：结束时连同子进程（go run 编出的二进制、vite 的子进程）一起停
  const p = spawn(cmd, args, { cwd, env: { ...process.env, ...env }, stdio: ['ignore', fd, fd], detached: true })
  // 启动失败（命令不存在等）记到日志里，由 waitFor 发现进程已退出
  p.on('error', (e) => appendFileSync(log, `\n启动失败：${e.message}\n`))
  return p
}

function stop(p: ChildProcess, signal: NodeJS.Signals): Promise<void> {
  if (p.exitCode !== null || p.pid === undefined) return Promise.resolve()
  return new Promise((ok) => {
    const t = setTimeout(() => {
      try {
        process.kill(-p.pid!, 'SIGKILL')
      } catch {
        /* 已退出 */
      }
      ok()
    }, 20_000)
    p.once('exit', () => {
      clearTimeout(t)
      ok()
    })
    try {
      process.kill(-p.pid!, signal)
    } catch {
      clearTimeout(t)
      ok()
    }
  })
}

export default async function globalSetup() {
  if (process.env.ISP_E2E_STATE) {
    if (!existsSync(process.env.ISP_E2E_STATE)) throw new Error(`ISP_E2E_STATE 指向的文件不存在：${process.env.ISP_E2E_STATE}`)
    return
  }

  const dir = join(repo, 'var', 'devstack-e2e')
  const logs = join(repo, 'var', 'e2e-logs')
  rmSync(logs, { recursive: true, force: true })
  mkdirSync(logs, { recursive: true })
  const [control, portal, admin] = [await freePort(), await freePort(), await freePort()]
  const controlUrl = `http://127.0.0.1:${control}`
  const portalUrl = `http://127.0.0.1:${portal}`
  const adminUrl = `http://127.0.0.1:${admin}`

  const procs: ChildProcess[] = []
  // 兜底：Playwright 进程异常退出时也发出停止信号（开发栈收到 SIGINT 会自行清理）
  process.once('exit', () => {
    for (const p of procs) {
      try {
        if (p.exitCode === null && p.pid) process.kill(-p.pid, 'SIGINT')
      } catch {
        /* 已退出 */
      }
    }
  })
  const teardown = async () => {
    // 前端先停；开发栈收到 SIGINT 后自行停 gost、isp-control、PostgreSQL 并删除目录
    await Promise.all(procs.slice(1).map((p) => stop(p, 'SIGTERM')))
    if (procs[0]) await stop(procs[0], 'SIGINT')
  }

  try {
    const devLog = join(logs, 'devstack.log')
    const dev = start(
      'go',
      ['run', './cmd/isp-devstack', '-dir', dir, '-listen', `127.0.0.1:${control}`, '-port-start', process.env.ISP_E2E_PORT_START ?? '24000', '-port-count', '200', '-portal-url', portalUrl, '-admin-url', adminUrl, '-quiet'],
      repo,
      {},
      devLog,
    )
    procs.push(dev)
    const state = join(dir, 'state.json')
    // 首次需要编译 isp-control 与 gost
    await waitFor('开发栈', () => existsSync(state) && JSON.parse(readFileSync(state, 'utf8')).controlUrl === controlUrl, 240_000, dev, devLog)

    const vite = (app: string) => join(web, 'apps', app, 'node_modules', '.bin', 'vite')
    const common = { ISP_CONTROL_URL: controlUrl, VITE_PORTAL_POLL_MS: '2000', BROWSER: 'none' }
    const pLog = join(logs, 'portal-vite.log')
    const aLog = join(logs, 'admin-vite.log')
    const p = start(vite('portal'), [], join(web, 'apps', 'portal'), { ...common, PORTAL_PORT: String(portal) }, pLog)
    procs.push(p)
    const a = start(vite('admin'), [], join(web, 'apps', 'admin'), { ...common, ADMIN_PORT: String(admin) }, aLog)
    procs.push(a)
    const up = (url: string) => async () => (await fetch(url)).ok
    await waitFor('用户前台 Vite', up(portalUrl), 60_000, p, pLog)
    await waitFor('管理后台 Vite', up(adminUrl), 60_000, a, aLog)
    // 代理是否通到控制面
    await waitFor('前台代理到控制面', async () => (await fetch(`${portalUrl}/api/catalog`)).status === 200, 30_000, p, pLog)

    process.env.ISP_E2E_STATE = state
  } catch (e) {
    await teardown()
    throw e
  }
  return teardown
}
