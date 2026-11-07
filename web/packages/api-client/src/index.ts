// 用户前台 API 契约（原型手写版）。
// 正式实现由 Go 后端产出 OpenAPI，再生成本文件中的类型；字段语义以 docs/01 为准。
// 约定：时间为 ISO 8601 字符串；流量为字节；金额为最小货币单位（分）。
// 剩余时长由服务端按自身时钟算出（remainingSeconds），前端不依赖本机时钟判断可用性。

export type Lifecycle = 'pending' | 'fulfill_failed' | 'active' | 'retained' | 'released'

/** 阻断原因，数组按 docs/01 §7.2 的展示优先级排列；空数组表示可用。 */
export type Blocker =
  | 'account_banned'
  | 'released'
  | 'admin_paused'
  | 'pending'
  | 'fulfill_failed'
  | 'expired'
  | 'traffic_exhausted'
  | 'changing'
  | 'dataplane'

export type ExitCheck = 'ok' | 'slow' | 'failed' | 'stale'

export interface Exit {
  version: number
  ip: string
  /** ISO 3166-1 alpha-2，小写，例如 us；用于国旗与筛选。 */
  countryCode: string
  country: string
  city: string
  check: ExitCheck
  latencyMs: number | null
  checkedAt: string
}

export type AccessField = 'ports' | 'password'

export interface Access {
  version: number
  host: string
  username: string
  password: string
  ports: { http: number; https: number; socks5: number }
  issuedAt: string
  /** 最近一次换接入改动、用户尚未确认的字段。 */
  unseenChanges: AccessField[]
}

export interface Subscription {
  id: string
  name: string
  plan: { name: string; countryCode: string; country: string; city: string; concurrency: number; bandwidthMbps: number }
  lifecycle: Lifecycle
  blockers: Blocker[]
  time: {
    periodDays: number
    startedAt: string | null
    expiresAt: string | null
    remainingSeconds: number
    retentionEndsAt: string | null
  }
  traffic: { totalBytes: number; usedBytes: number }
  exit: Exit | null
  access: Access | null
  quotas: {
    accessChangesLeft: number
    accessChangesPerDay: number
    freeExitChangesLeft: number
    freeExitWindowEndsAt: string | null
  }
  openExitRequestId: string | null
  openOrderIds: string[]
}

export type SubscriptionEventKind =
  | 'ready'
  | 'time_added'
  | 'traffic_added'
  | 'password_reset'
  | 'access_changed'
  | 'exit_changed'
  | 'expired'
  | 'exhausted'
  | 'renamed'

export interface SubscriptionEvent {
  id: string
  at: string
  kind: SubscriptionEventKind
  detail: string
  orderNo?: string
}

export interface Money {
  amount: number
  currency: 'CNY'
}

export type StockLevel = 'plenty' | 'low' | 'out'

export interface Plan {
  id: string
  /** ISO 3166-1 alpha-2，小写。 */
  countryCode: string
  country: string
  city: string
  monthlyPrice: Money
  includedBytes: number
  concurrency: number
  bandwidthMbps: number
  stock: StockLevel
}

export interface TrafficPack {
  id: string
  bytes: number
  price: Money
}

export interface Rules {
  version: string
  periodDays: number
  retentionDays: number
  accessChangesPerDay: number
  freeExitChangesPer30Days: number
  unpaidHoldHours: number
  maxUnpaidNewOrders: number
}

export interface Catalog {
  plans: Plan[]
  trafficPacks: TrafficPack[]
  monthOptions: number[]
  rules: Rules
  payment: { payee: string; bank: string; account: string; note: string }
}

export type OrderKind = 'new' | 'renew_time' | 'add_traffic'
export type PaymentStatus = 'awaiting' | 'confirmed' | 'cancelled' | 'refund_review' | 'refunded'
export type FulfillmentStatus = 'not_started' | 'processing' | 'delivered' | 'failed'

export interface OrderLine {
  label: string
  detail: string
  amount: Money
}

export interface OrderStep {
  label: string
  state: 'done' | 'current' | 'todo' | 'failed'
  at: string | null
}

export interface Order {
  id: string
  no: string
  kind: OrderKind
  createdAt: string
  holdUntil: string | null
  amount: Money
  payment: PaymentStatus
  fulfillment: FulfillmentStatus
  userClaimedPaidAt: string | null
  payRef: string
  subscriptionId: string | null
  subscriptionName: string | null
  lines: OrderLine[]
  steps: OrderStep[]
  /** 下单时保存的规则快照，套餐改价或下架不追溯（R-09.9）。 */
  rulesSnapshot: string[]
}

export type OrderInput =
  | { kind: 'new'; planId: string; months: number; quantity: number }
  | { kind: 'renew_time'; subscriptionId: string; months: number }
  | { kind: 'add_traffic'; subscriptionId: string; packId: string }

export type ExitReason = 'target_blocked' | 'quality' | 'region' | 'other'
export type ExitRequestStatus = 'pending' | 'processing' | 'succeeded' | 'failed' | 'cancelled'

export interface ExitRequest {
  id: string
  no: string
  subscriptionId: string
  subscriptionName: string
  createdAt: string
  updatedAt: string
  reason: ExitReason
  note: string
  alsoChangeAccess: boolean
  charge: 'free' | 'manual'
  status: ExitRequestStatus
  oldIp: string
  newIp: string | null
  failureReason: string | null
}

export interface ExitRequestInput {
  subscriptionId: string
  reason: ExitReason
  note: string
  alsoChangeAccess: boolean
}

export interface Account {
  email: string
  verified: boolean
  status: 'active' | 'banned'
  createdAt: string
}

/** 错误码，与 OpenAPI ErrorCode 完全一致（forbidden：CSRF 校验失败或管理端权限不足）。 */
export type ApiErrorCode =
  | 'unauthorized'
  | 'unverified'
  | 'forbidden'
  | 'rate_limited'
  | 'conflict'
  | 'quota_exhausted'
  | 'not_found'
  | 'invalid'
  | 'precondition'

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

export interface PortalApi {
  session(): Promise<Account | null>
  login(email: string, password: string): Promise<Account>
  register(email: string, password: string): Promise<{ email: string }>
  verifyEmail(code: string): Promise<Account>
  resendVerification(): Promise<void>
  requestPasswordReset(email: string): Promise<void>
  changePassword(current: string, next: string): Promise<void>
  logout(): Promise<void>

  listSubscriptions(): Promise<Subscription[]>
  getSubscription(id: string): Promise<Subscription>
  listSubscriptionEvents(id: string): Promise<SubscriptionEvent[]>
  renameSubscription(id: string, name: string): Promise<Subscription>
  changeAccess(id: string, mode: 'password' | 'ports_and_password'): Promise<Access>
  acknowledgeAccess(id: string): Promise<void>

  getCatalog(): Promise<Catalog>
  createOrder(input: OrderInput): Promise<Order>
  listOrders(): Promise<Order[]>
  getOrder(id: string): Promise<Order>
  claimPaid(id: string): Promise<Order>
  cancelOrder(id: string): Promise<Order>

  listExitRequests(): Promise<ExitRequest[]>
  createExitRequest(input: ExitRequestInput): Promise<ExitRequest>
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
