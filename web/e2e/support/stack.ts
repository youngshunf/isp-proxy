// 开发栈的就绪信息（cmd/isp-devstack 写出的 state.json）与几个跨用例的工具：
// 读邮件验证码、算管理员 TOTP、经接入发 HTTP 请求。
import { createHmac } from 'node:crypto'
import { existsSync, readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs'
import { request as httpRequest } from 'node:http'
import { connect } from 'node:net'
import { join } from 'node:path'

export interface Exit {
  label: string
  ip: string
  country: string
  city: string
  url: string
  resourceId?: string
  /** 绑定上限（D-24）：1 独享，≥2 可共享。 */
  maxTenants: number
}

export interface StackState {
  controlUrl: string
  portalUrl: string
  adminUrl: string
  mailDir: string
  dir: string
  admin: { email: string; password: string }
  user: { email: string; password: string }
  totpSecret: string
  totpLastStep: number
  nodeId: string
  nodeHost: string
  exits: Exit[]
  spares: Exit[]
  /** 经接入访问的目标（127.0.0.1 上的回显服务；假上游一律连到自己的后端，/ip 返回出口标签）。 */
  probeUrl: string
}

export function stack(): StackState {
  const p = process.env.ISP_E2E_STATE
  if (!p) throw new Error('ISP_E2E_STATE 未设置：用 pnpm e2e 运行（globalSetup 会启动开发栈）')
  return JSON.parse(readFileSync(p, 'utf8')) as StackState
}

export const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

/** 每个用例用一个新邮箱，互不干扰。 */
export function newEmail(tag: string): string {
  return `e2e-${tag}-${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}@example.test`
}

// ---------- 邮件 ----------

interface Mail {
  to: string
  subject: string
  body: string
  mtime: number
}

function decodeSubject(s: string): string {
  return s.replace(/=\?utf-8\?b\?([^?]*)\?=/gi, (_, b64: string) => Buffer.from(b64, 'base64').toString('utf8')).replace(/\?=\s+=\?/g, '')
}

function decodeQP(s: string): string {
  const soft = s.replace(/=\r?\n/g, '')
  const bytes: number[] = []
  for (let i = 0; i < soft.length; i++) {
    const c = soft[i]!
    if (c === '=' && /^[0-9A-F]{2}$/i.test(soft.slice(i + 1, i + 3))) {
      bytes.push(parseInt(soft.slice(i + 1, i + 3), 16))
      i += 2
    } else bytes.push(...Buffer.from(c, 'utf8'))
  }
  return Buffer.from(bytes).toString('utf8').replace(/\r\n/g, '\n')
}

function readMails(dir: string): Mail[] {
  if (!existsSync(dir)) return []
  return readdirSync(dir)
    .filter((f) => f.endsWith('.eml'))
    .map((f) => {
      const path = join(dir, f)
      const raw = readFileSync(path, 'utf8')
      const cut = raw.indexOf('\r\n\r\n')
      const head = raw.slice(0, cut)
      // 多行头（RFC 5322 折行）先展开
      const headers = head.replace(/\r\n[ \t]+/g, ' ').split('\r\n')
      const get = (k: string) => headers.find((h) => h.toLowerCase().startsWith(k.toLowerCase() + ':'))?.slice(k.length + 1).trim() ?? ''
      return { to: get('To'), subject: decodeSubject(get('Subject')), body: decodeQP(raw.slice(cut + 4)), mtime: statSync(path).mtimeMs }
    })
}

/** 等待发给 to、主题含 subject 的最新一封邮件，返回其中的 6 位验证码。 */
export async function mailCode(to: string, subject: string, after = 0, timeoutMs = 15_000): Promise<string> {
  const { mailDir } = stack()
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    const hit = readMails(mailDir)
      .filter((m) => m.to.toLowerCase().includes(to.toLowerCase()) && m.subject.includes(subject) && m.mtime >= after)
      .sort((a, b) => b.mtime - a.mtime)[0]
    const code = hit?.body.match(/^\s+([0-9]{6})\s*$/m)?.[1]
    if (code) return code
    await sleep(200)
  }
  throw new Error(`${timeoutMs}ms 内没有收到发给 ${to} 的“${subject}”邮件（${mailDir}）`)
}

// ---------- 管理员 TOTP ----------

function base32(s: string): Buffer {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  let bits = ''
  for (const c of s.replace(/=+$/, '').toUpperCase()) bits += alphabet.indexOf(c).toString(2).padStart(5, '0')
  const out: number[] = []
  for (let i = 0; i + 8 <= bits.length; i += 8) out.push(parseInt(bits.slice(i, i + 8), 2))
  return Buffer.from(out)
}

function hotp(secret: Buffer, counter: number): string {
  const msg = Buffer.alloc(8)
  msg.writeBigUInt64BE(BigInt(counter))
  const h = createHmac('sha1', secret).update(msg).digest()
  const off = h[h.length - 1]! & 0xf
  const n = (h.readUInt32BE(off) & 0x7fffffff) % 1_000_000
  return n.toString().padStart(6, '0')
}

/**
 * 下一个可用的验证码。服务端接受当前时间步 ±1，且同一时间步只能用一次（防重放），
 * 所以记住用过的最大时间步（文件，跨 worker），必要时等下一个时间步。
 */
export async function nextTotp(): Promise<string> {
  const st = stack()
  const file = join(st.dir, 'e2e-totp-step')
  const last = existsSync(file) ? Number(readFileSync(file, 'utf8')) : st.totpLastStep
  for (;;) {
    const cur = Math.floor(Date.now() / 1000 / 30)
    const step = [cur - 1, cur, cur + 1].find((s) => s > last)
    // 留一点余量：cur+1 只在本步后半段才用，避免与服务端时钟的细微差错位
    if (step !== undefined && (step <= cur || (Date.now() / 1000) % 30 > 5)) {
      writeFileSync(file, String(step))
      return hotp(base32(st.totpSecret), step)
    }
    await sleep(1000)
  }
}

// ---------- 经接入发请求 ----------

export interface ProxyResult {
  status: number
  body: string
}

/** 把 http://user:pass@host:port 或 socks5://… 连接串当代理，GET 一个 http 地址。 */
export function viaProxy(conn: string, target = stack().probeUrl, timeoutMs = 8000): Promise<ProxyResult> {
  const u = new URL(conn)
  const user = decodeURIComponent(u.username)
  const pass = decodeURIComponent(u.password)
  if (u.protocol === 'socks5:') return viaSocks5(u.hostname, Number(u.port), user, pass, target, timeoutMs)
  if (u.protocol !== 'http:') throw new Error(`viaProxy 只支持 http 与 socks5：${u.protocol}`)
  return new Promise((resolve, reject) => {
    const req = httpRequest({
      host: u.hostname,
      port: Number(u.port),
      method: 'GET',
      path: target,
      headers: { Host: new URL(target).host, 'Proxy-Authorization': 'Basic ' + Buffer.from(`${user}:${pass}`).toString('base64') },
      timeout: timeoutMs,
    })
    req.on('response', (res) => {
      let body = ''
      res.setEncoding('utf8')
      res.on('data', (c: string) => (body += c))
      res.on('end', () => resolve({ status: res.statusCode ?? 0, body: body.trim() }))
    })
    req.on('timeout', () => req.destroy(new Error('代理请求超时')))
    req.on('error', reject)
    req.end()
  })
}

function viaSocks5(host: string, port: number, user: string, pass: string, target: string, timeoutMs: number): Promise<ProxyResult> {
  const t = new URL(target)
  return new Promise((resolve, reject) => {
    const sock = connect(port, host)
    sock.setTimeout(timeoutMs, () => sock.destroy(new Error('SOCKS5 请求超时')))
    let stage = 0
    let buf = Buffer.alloc(0)
    sock.on('error', reject)
    sock.on('close', () => {
      if (stage < 3) return resolve({ status: 0, body: '' })
      const text = buf.toString('utf8')
      const status = Number(text.match(/^HTTP\/1\.[01] (\d{3})/)?.[1] ?? 0)
      resolve({ status, body: text.slice(text.indexOf('\r\n\r\n') + 4).trim() })
    })
    sock.on('connect', () => sock.write(Buffer.from([5, 1, 2])))
    sock.on('data', (d) => {
      buf = Buffer.concat([buf, d])
      if (stage === 0 && buf.length >= 2) {
        if (buf[1] !== 2) return sock.destroy()
        buf = buf.subarray(2)
        stage = 1
        const ub = Buffer.from(user)
        const pb = Buffer.from(pass)
        sock.write(Buffer.concat([Buffer.from([1, ub.length]), ub, Buffer.from([pb.length]), pb]))
      } else if (stage === 1 && buf.length >= 2) {
        if (buf[1] !== 0) return sock.destroy() // 认证被拒
        buf = buf.subarray(2)
        stage = 2
        const hb = Buffer.from(t.hostname)
        const pt = Buffer.alloc(2)
        pt.writeUInt16BE(Number(t.port || 80))
        sock.write(Buffer.concat([Buffer.from([5, 1, 0, 3, hb.length]), hb, pt]))
      } else if (stage === 2 && buf.length >= 10) {
        if (buf[1] !== 0) return sock.destroy()
        buf = Buffer.alloc(0)
        stage = 3
        sock.write(`GET ${t.pathname}${t.search} HTTP/1.1\r\nHost: ${t.host}\r\nConnection: close\r\n\r\n`)
      }
    })
  })
}

/** 等到经接入能拿到期望的出口标签（下发到节点有几秒延迟）。 */
export async function expectExit(conn: string, label: string, timeoutMs = 20_000): Promise<void> {
  const deadline = Date.now() + timeoutMs
  let last = ''
  while (Date.now() < deadline) {
    try {
      const r = await viaProxy(conn)
      if (r.status === 200 && r.body === label) return
      last = `${r.status} ${r.body}`
    } catch (e) {
      last = String(e)
    }
    await sleep(500)
  }
  throw new Error(`经 ${redact(conn)} 没有到达出口 ${label}（最后一次：${last}）`)
}

/** 等到旧接入被拒（代理认证失败或连接被断）。 */
export async function expectRejected(conn: string, timeoutMs = 20_000): Promise<void> {
  const deadline = Date.now() + timeoutMs
  let last = ''
  while (Date.now() < deadline) {
    try {
      const r = await viaProxy(conn)
      if (r.status !== 200) return
      last = `${r.status} ${r.body}`
    } catch {
      return
    }
    await sleep(500)
  }
  throw new Error(`旧接入 ${redact(conn)} 仍可用（${last}）`)
}

function redact(conn: string): string {
  return conn.replace(/:[^:@/]+@/, ':***@')
}
