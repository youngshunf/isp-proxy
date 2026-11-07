// 原型阶段使用内存 mock；接入真实后端时替换为 OpenAPI 生成的客户端，接口签名不变。
import { ApiError, type PortalApi } from '@isp/api-client'
import { MOCK_NOW, createMockApi } from '@isp/api-client/mock'

export const api: PortalApi = createMockApi()

/** 服务端时间。正式实现取响应头 Date 与本机时钟的偏移；原型固定在 mock 的“当前时间”。 */
export function serverNow(): number {
  return MOCK_NOW
}

export function errorText(e: unknown): string {
  if (e instanceof ApiError) return e.message
  return '请求没有完成，请检查网络后重试'
}

export const qk = {
  session: ['session'] as const,
  subs: ['subs'] as const,
  sub: (id: string) => ['subs', id] as const,
  events: (id: string) => ['subs', id, 'events'] as const,
  catalog: ['catalog'] as const,
  orders: ['orders'] as const,
  order: (id: string) => ['orders', id] as const,
  requests: ['requests'] as const,
}
