// 用户前台的数据入口（docs/05 §6）。
// 默认用真实客户端（同源 /api：开发时 Vite 代理、生产时反向代理接到 isp-control）；
// VITE_PORTAL_API=mock 时改用内存 mock（演示与截图），组件测试经 configureApi 注入新的 mock 实例。
import { ApiError, type PortalApi } from '@isp/api-client'
import { NetworkError, createServerClock } from '@isp/api-client/http'
import { MOCK_NOW, createMockApi } from '@isp/api-client/mock'
import { createPortalApi } from '@isp/api-client/portal'

export const USE_MOCK = import.meta.env.VITE_PORTAL_API === 'mock'

/** 待核款、开通中、处理中、变更中这类状态的轮询间隔（docs/05 §6 第 5 步：15～30 秒）；E2E 可调小。 */
export const POLL_MS = Number(import.meta.env.VITE_PORTAL_POLL_MS) || 20_000

// ES 模块的 live binding：页面 import { api } 总是拿到当前实例。
export let api: PortalApi
let nowFn: () => number

/** 替换数据源（测试注入 mock；now 为“服务端当前时间”）。 */
export function configureApi(next: PortalApi, now: () => number): void {
  api = next
  nowFn = now
}

function configureDefault() {
  if (USE_MOCK) {
    configureApi(createMockApi(), () => MOCK_NOW)
    return
  }
  // 服务端时间：响应头 Date 与本机时钟的偏移，只用于“几分钟前”这类展示；可用性永远读服务端字段。
  const clock = createServerClock()
  configureApi(createPortalApi({ onServerTime: (ms) => clock.observe(ms) }), () => clock.now())
}
configureDefault()

/** 服务端当前时间（只用于展示）。 */
export function serverNow(): number {
  return nowFn()
}

export function errorText(e: unknown): string {
  if (e instanceof ApiError) return e.message
  if (e instanceof NetworkError) return e.message
  return '请求没有完成，请检查网络后重试'
}

// ---- 查询键：带当前用户标识，换账号不会读到上一个账号的缓存（docs/05 §6 第 2 步） ----
let userKey = 'anon'

/** 会话确定后设置（Shell、登录、验证邮箱成功时）；退出、改密、会话失效时连同 queryClient.clear() 一起重置。 */
export function setQueryUser(email: string | null | undefined): void {
  userKey = email ? email.toLowerCase() : 'anon'
}

export const qk = {
  /** 会话本身不按用户区分（它决定当前用户是谁）。 */
  session: ['session'] as const,
  /** 目录对访客公开，与用户无关。 */
  catalog: ['catalog'] as const,
  get subs() {
    return ['u', userKey, 'subs'] as const
  },
  sub: (id: string) => ['u', userKey, 'subs', id] as const,
  events: (id: string) => ['u', userKey, 'subs', id, 'events'] as const,
  get orders() {
    return ['u', userKey, 'orders'] as const
  },
  order: (id: string) => ['u', userKey, 'orders', id] as const,
  get requests() {
    return ['u', userKey, 'requests'] as const
  },
}
