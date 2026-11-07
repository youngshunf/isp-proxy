// 用户前台 API：实体类型全部取自 api/openapi.yaml 生成的 schema.ts（F-W1），字段语义以 docs/01 为准。
// 真实客户端见 createPortalApi（./portal），内存 mock 见 '@isp/api-client/mock'，两者实现同一个 PortalApi。
// 约定：时间为 ISO 8601 字符串；流量为字节；金额为最小货币单位（分）。
// 剩余时长由服务端按自身时钟算出（remainingSeconds），前端不依赖本机时钟判断可用性。

import type { components } from './generated/schema'

type S = components['schemas']

export type Lifecycle = S['Lifecycle']
/** 阻断原因，数组按 docs/01 §7.2 的展示优先级排列；空数组表示可用。 */
export type Blocker = S['Blocker']
export type ExitCheck = S['ExitCheck']
export type Exit = S['Exit']
export type AccessField = S['AccessField']
export type Access = S['Access']
export type Subscription = S['Subscription']
export type SubscriptionEventKind = S['SubscriptionEventKind']
export type SubscriptionEvent = S['SubscriptionEvent']
export type Money = S['Money']
export type StockLevel = S['StockLevel']
export type Plan = S['Plan']
export type TrafficPack = S['TrafficPack']
export type Rules = S['Rules']
export type PaymentInstructions = S['PaymentInstructions']
export type Catalog = S['Catalog']
export type OrderKind = S['OrderKind']
export type PaymentStatus = S['PaymentStatus']
export type FulfillmentStatus = S['FulfillmentStatus']
export type OrderLine = S['OrderLine']
export type OrderStep = S['OrderStep']
export type Order = S['Order']
/** 下单入参。新购 quantity 只能为 1（D-13，一次订单一个订阅）。 */
export type OrderInput = S['OrderInput']
export type ExitReason = S['ExitReason']
export type ExitRequestStatus = S['ExitRequestStatus']
export type ExitRequest = S['ExitRequest']
export type ExitRequestInput = S['ExitRequestInput']
export type Account = S['Account']
export type ChangeAccessMode = S['ChangeAccessInput']['mode']

/** 错误码，与 OpenAPI ErrorCode 完全一致（forbidden：CSRF 校验失败或管理端权限不足）。 */
export type ApiErrorCode = S['ErrorCode']

/** 服务端给出的可选恢复动作，例如“去续时长”。 */
export interface ApiErrorAction {
  label: string
  href: string
}

export class ApiError extends Error {
  readonly code: ApiErrorCode
  readonly action: ApiErrorAction | null
  /** HTTP 状态；mock 抛出时按错误码推断。 */
  readonly status: number
  /** rate_limited 时的 Retry-After 秒数。 */
  readonly retryAfter: number | null
  /** 409 冲突时服务端附带的现有对象（如换出口申请的 existing）。 */
  readonly details: Record<string, unknown> | null
  constructor(
    code: ApiErrorCode,
    message: string,
    extra: { action?: ApiErrorAction | null; status?: number; retryAfter?: number | null; details?: Record<string, unknown> | null } = {},
  ) {
    super(message)
    this.code = code
    this.name = 'ApiError'
    this.action = extra.action ?? null
    this.status = extra.status ?? STATUS_OF[code]
    this.retryAfter = extra.retryAfter ?? null
    this.details = extra.details ?? null
  }
}

export const STATUS_OF: Record<ApiErrorCode, number> = {
  unauthorized: 401,
  unverified: 403,
  forbidden: 403,
  not_found: 404,
  conflict: 409,
  precondition: 412,
  invalid: 422,
  quota_exhausted: 429,
  rate_limited: 429,
}

/**
 * 用户前台的全部业务接口，方法名即 operationId（docs/05 §5）。
 * 带幂等键的写操作（下单、换接入、换出口申请）由调用方按“一次用户意图一个键”传入，重试时复用。
 */
export interface PortalApi {
  /** GET /api/me；未登录（401）返回 null。 */
  session(): Promise<Account | null>
  login(email: string, password: string): Promise<Account>
  /** 注册不建会话（不泄露邮箱是否已注册）；随后用 verifyEmail(code, email) 验证即登录。 */
  register(email: string, password: string): Promise<{ email: string }>
  /** 已登录（未验证）时只传 code；未登录时必须带 email，成功即登录。 */
  verifyEmail(code: string, email?: string): Promise<Account>
  /** 已登录时发给本人；未登录时传 email（结果一律成功，不泄露是否注册）。 */
  resendVerification(email?: string): Promise<void>
  requestPasswordReset(email: string): Promise<void>
  /** 用邮件验证码设置新密码，成功后撤销该账号全部会话（需重新登录）。 */
  confirmPasswordReset(email: string, code: string, newPassword: string): Promise<void>
  /** 改密撤销其他会话，当前会话换发新令牌（仍保持登录）。 */
  changePassword(current: string, next: string): Promise<void>
  logout(): Promise<void>

  listSubscriptions(): Promise<Subscription[]>
  getSubscription(id: string): Promise<Subscription>
  listSubscriptionEvents(id: string): Promise<SubscriptionEvent[]>
  renameSubscription(id: string, name: string): Promise<Subscription>
  changeAccess(id: string, mode: ChangeAccessMode, idempotencyKey: string): Promise<Access>
  acknowledgeAccess(id: string): Promise<void>

  getCatalog(): Promise<Catalog>
  createOrder(input: OrderInput, idempotencyKey: string): Promise<Order>
  listOrders(): Promise<Order[]>
  getOrder(id: string): Promise<Order>
  claimPaid(id: string): Promise<Order>
  cancelOrder(id: string): Promise<Order>

  listExitRequests(): Promise<ExitRequest[]>
  createExitRequest(input: ExitRequestInput, idempotencyKey: string): Promise<ExitRequest>
}

/** 每次用户意图一个幂等键（打开对话框或进入下单页时生成，重试复用，成功后换新）。 */
export function newIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && 'randomUUID' in crypto) return crypto.randomUUID()
  return `k-${Date.now()}-${Math.random().toString(36).slice(2)}`
}

/** 订单是否还会变化（待核款、开通中/入账中）；终态时停止轮询。 */
export function orderInFlight(o: Pick<Order, 'payment' | 'fulfillment'>): boolean {
  if (o.payment === 'awaiting' || o.payment === 'refund_review') return true
  return o.payment === 'confirmed' && (o.fulfillment === 'not_started' || o.fulfillment === 'processing')
}

/** 换出口申请是否未完成。 */
export function exitRequestOpen(r: Pick<ExitRequest, 'status'>): boolean {
  return r.status === 'pending' || r.status === 'processing'
}

export type Protocol = 'http' | 'https' | 'socks5'

export const PROTOCOLS: readonly Protocol[] = ['http', 'https', 'socks5']

/** 客户程序使用的连接串。用户名与密码按 RFC 3986 做百分号编码。 */
export function connectionString(access: Access, proto: Protocol): string {
  const user = encodeURIComponent(access.username)
  const pass = encodeURIComponent(access.password)
  return `${proto}://${user}:${pass}@${access.host}:${access.ports[proto]}`
}

export function remainingBytes(sub: Pick<Subscription, 'traffic'>): number {
  return Math.max(0, sub.traffic.totalBytes - sub.traffic.usedBytes)
}
