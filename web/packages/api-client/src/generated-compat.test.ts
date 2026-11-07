// 类型层契约测试：手写原型类型（src/index.ts）与从 api/openapi.yaml 生成的类型（src/generated/schema.ts）是否兼容。
// 断言在 `tsc`（pnpm typecheck）阶段生效；vitest 只跑下面的占位用例。
//
// 方向：
//   - 服务端返回的类型：生成类型必须能赋给手写类型（前端按手写类型消费真实响应不会出错）。
//   - 客户端提交的类型：手写类型必须能赋给生成类型（原型的请求体是合法的契约请求体）。
//
// 已知差异（F-W1 用生成类型替换手写类型时处理）：
//   1. 生成的 Plan / TrafficPack 多一个 versionId（下单时可回传做价格版本校验）；OrderInput 多可选的
//      planVersionId / packVersionId，renew_time 多可选 packId（含流量续费）。
//   2. 生成的 ErrorCode 多一个 forbidden（CSRF 校验失败、管理端权限）；手写 ApiErrorCode 没有。
//   3. verifyEmail / resendVerification 的请求体多可选 email（未登录时使用）；新增 confirmPasswordReset。
//   4. OrderInputNew.quantity 契约上限为 1（D-13 一次订单一个订阅），原型购买页的数量步进器在 F-W4 删除。
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

// 错误码：手写是生成的子集，唯一多出的是 forbidden（差异 2）。
assert<Assignable<H.ApiErrorCode, S['ErrorCode']>>()
assert<Equal<Exclude<S['ErrorCode'], H.ApiErrorCode>, 'forbidden'>>()

// ---- 请求：手写 → 生成 ----
assert<Assignable<H.OrderInput, Body<'createOrder'>>>()
assert<Assignable<H.ExitRequestInput, Body<'createExitRequest'>>>()
assert<Assignable<{ mode: 'password' | 'ports_and_password' }, Body<'changeAccess'>>>()
assert<Assignable<{ name: string }, Body<'renameSubscription'>>>()
assert<Assignable<{ email: string; password: string }, Body<'login'>>>()
assert<Assignable<{ code: string }, Body<'verifyEmail'>>>()
assert<Assignable<{ current: string; next: string }, Body<'changePassword'>>>()

describe('生成类型与原型类型', () => {
  it('类型层断言由 tsc 检查', () => {
    expect(true).toBe(true)
  })
})
