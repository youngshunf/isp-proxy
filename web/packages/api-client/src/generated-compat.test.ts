// 类型层契约测试：src/index.ts 导出的实体类型（F-W1 起直接取自生成的 schema.ts）与契约保持一致。
// 断言在 `tsc`（pnpm typecheck）阶段生效；vitest 只跑下面的占位用例。
//
// 方向：
//   - 服务端返回的类型：生成类型必须能赋给前端使用的类型。
//   - 客户端提交的类型：前端构造的请求体必须是合法的契约请求体。
//
// 原型阶段的已知差异已全部消除：versionId、含流量续费的 packId、未登录验证带 email、confirmPasswordReset、
// 新购 quantity 只能为 1（购买页已删除数量步进器）、目录带品牌名 brand。
import { describe, expect, it } from 'vitest'
import type * as H from './index'
import type { components, operations } from './generated/schema'

type S = components['schemas']
type Json<Op extends keyof operations, Code extends number> = operations[Op]['responses'] extends Record<
  Code,
  { content: { 'application/json': infer T } }
>
  ? T
  : never
type Body<Op extends keyof operations> = operations[Op]['requestBody'] extends
  | { content: { 'application/json': infer T } }
  | undefined
  ? T
  : never

type Assignable<From, To> = [From] extends [To] ? true : false
type Equal<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false
function assert<T extends true>(): T | undefined {
  return undefined
}

// ---- 响应：生成 → 手写 ----
assert<Assignable<S['Subscription'], H.Subscription>>()
assert<Assignable<S['Access'], H.Access>>()
assert<Assignable<S['Exit'], H.Exit>>()
assert<Assignable<S['SubscriptionEvent'], H.SubscriptionEvent>>()
assert<Assignable<S['Catalog'], H.Catalog>>()
assert<Assignable<S['Order'], H.Order>>()
assert<Assignable<S['ExitRequest'], H.ExitRequest>>()
assert<Assignable<S['Account'], H.Account>>()
assert<Assignable<Json<'listSubscriptions', 200>, H.Subscription[]>>()
assert<Assignable<Json<'changeAccess', 200>, H.Access>>()
assert<Assignable<Json<'createOrder', 201>, H.Order>>()
assert<Assignable<Json<'createExitRequest', 201>, H.ExitRequest>>()
assert<Assignable<Json<'register', 201>, { email: string }>>()

// 枚举完全一致（状态词不得漂移）。
assert<Equal<S['Lifecycle'], H.Lifecycle>>()
assert<Equal<S['Blocker'], H.Blocker>>()
assert<Equal<S['ExitCheck'], H.ExitCheck>>()
assert<Equal<S['SubscriptionEventKind'], H.SubscriptionEventKind>>()
assert<Equal<S['OrderKind'], H.OrderKind>>()
assert<Equal<S['PaymentStatus'], H.PaymentStatus>>()
assert<Equal<S['FulfillmentStatus'], H.FulfillmentStatus>>()
assert<Equal<S['ExitReason'], H.ExitReason>>()
assert<Equal<S['ExitRequestStatus'], H.ExitRequestStatus>>()
assert<Equal<S['StockLevel'], H.StockLevel>>()
assert<Equal<S['AccessField'], H.AccessField>>()

// 错误码：完全一致。
assert<Equal<H.ApiErrorCode, S['ErrorCode']>>()

// ---- 请求：手写 → 生成 ----
assert<Assignable<H.OrderInput, Body<'createOrder'>>>()
assert<Assignable<H.ExitRequestInput, Body<'createExitRequest'>>>()
assert<Assignable<{ mode: 'password' | 'ports_and_password' }, Body<'changeAccess'>>>()
assert<Assignable<{ name: string }, Body<'renameSubscription'>>>()
assert<Assignable<{ email: string; password: string }, Body<'login'>>>()
assert<Assignable<{ code: string }, Body<'verifyEmail'>>>()
assert<Assignable<{ code: string; email: string }, Body<'verifyEmail'>>>()
assert<Assignable<{ email: string; code: string; newPassword: string }, Body<'confirmPasswordReset'>>>()
assert<Equal<H.Catalog['brand'], string>>()
assert<Assignable<{ current: string; next: string }, Body<'changePassword'>>>()

describe('生成类型与原型类型', () => {
  it('类型层断言由 tsc 检查', () => {
    expect(true).toBe(true)
  })
})
