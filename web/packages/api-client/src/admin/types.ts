// 管理端 API 的类型：全部从 api/openapi.yaml 生成的 schema.ts 推导，不手写字段。
// AdminApi 的方法名即 operationId（docs/05 §5），参数统一为一个对象：路径参数平铺，另加 query、body、idempotencyKey。
import type { components, operations } from '../generated/schema'
import type { OpInput, OpMethod, OpResult } from '../ops'

export type Schemas = components['schemas']

/** 管理端全部操作（operationId 以 admin 开头）。 */
export type AdminOp = Extract<keyof operations, `admin${string}`>

/** 一个操作的入参：路径参数 + query + body + 幂等键（推导见 ../ops.ts）。 */
export type AdminInput<O extends AdminOp> = OpInput<O>

/** 一个操作的成功响应体（204 为 void）。 */
export type AdminResult<O extends AdminOp> = OpResult<O>

export type AdminMethod<O extends AdminOp> = OpMethod<O>

/** 管理后台的全部业务接口。真实实现见 createAdminApi，内存实现见 createMockAdminApi。 */
export type AdminApi = { [O in AdminOp]: AdminMethod<O> }

// ---- 常用实体别名（页面直接用这些名字） ----
export type Admin = Schemas['Admin']
export type AdminRole = Schemas['AdminRole']
export type AdminLoginChallenge = Schemas['AdminLoginChallenge']
export type AdminSession = Schemas['AdminSession']
export type Overview = Schemas['Overview']
export type AdminLifecycle = Schemas['AdminLifecycle']
export type Blocker = Schemas['Blocker']
export type ExitCheck = Schemas['ExitCheck']
export type AdminSubscription = Schemas['AdminSubscription']
export type AdminSubscriptionDetail = Schemas['AdminSubscriptionDetail']
export type AdminSubscriptionEvent = Schemas['AdminSubscriptionEvent']
export type AdminExit = Schemas['AdminExit']
export type AdminAccess = Schemas['AdminAccess']
export type AccessSecret = Schemas['AccessSecret']
export type AccessVersion = Schemas['AccessVersion']
export type ExitBinding = Schemas['ExitBinding']
export type ExitCheckResult = Schemas['ExitCheckResult']
export type Task = Schemas['Task']
export type TaskKind = Schemas['TaskKind']
export type TaskState = Schemas['TaskState']
export type Probe = Schemas['Probe']
export type ExitComparison = Schemas['ExitComparison']
export type LedgerEntry = Schemas['LedgerEntry']
export type Ledger = Schemas['Ledger']
export type LedgerSourceType = Schemas['LedgerSourceType']
export type Transfer = Schemas['Transfer']
export type UsageSeries = Schemas['UsageSeries']
export type UsagePoint = Schemas['UsagePoint']
export type Upstream = Schemas['Upstream']
export type UpstreamStatus = Schemas['UpstreamStatus']
export type UpstreamInput = Schemas['UpstreamInput']
export type UpstreamDetail = Schemas['UpstreamDetail']
export type UpstreamOccupancy = Schemas['UpstreamOccupancy']
export type UpstreamTenant = Schemas['UpstreamTenant']
export type ExitMode = Schemas['ExitMode']
export type SaleStatus = Schemas['SaleStatus']
export type PlanVersion = Schemas['PlanVersion']
export type PlanVersionInput = Schemas['PlanVersionInput']
export type AdminPlan = Schemas['AdminPlan']
export type TrafficPackVersion = Schemas['TrafficPackVersion']
export type AdminTrafficPack = Schemas['AdminTrafficPack']
export type RulesInput = Schemas['RulesInput']
export type RulesVersion = Schemas['RulesVersion']
export type AdminUser = Schemas['AdminUser']
export type AdminUserDetail = Schemas['AdminUserDetail']
export type AdminUserDirect = Schemas['AdminUserDirect']
export type SubscriptionKind = Schemas['SubscriptionKind']
export type SubscriptionDirect = Schemas['SubscriptionDirect']
export type AdminOrder = Schemas['AdminOrder']
export type PaymentRecord = Schemas['PaymentRecord']
export type ConfirmPaymentInput = Schemas['ConfirmPaymentInput']
export type Refund = Schemas['Refund']
export type ExitFeeStatus = Schemas['ExitFeeStatus']
export type AdminExitRequest = Schemas['AdminExitRequest']
export type Node = Schemas['Node']
export type Alert = Schemas['Alert']
export type AlertKind = Schemas['AlertKind']
export type AlertSeverity = Schemas['AlertSeverity']
export type AlertStatus = Schemas['AlertStatus']
export type AuditLog = Schemas['AuditLog']
export type Actor = Schemas['Actor']
export type Money = Schemas['Money']
export type PaymentStatus = Schemas['PaymentStatus']
export type FulfillmentStatus = Schemas['FulfillmentStatus']
export type OrderKind = Schemas['OrderKind']
export type ExitRequestStatus = Schemas['ExitRequestStatus']
export type ExitReason = Schemas['ExitReason']

/** 管理端分页响应的公共形状（cursor + limit，docs/06 §8.2）。 */
export interface Page<T> {
  items: T[]
  nextCursor: string | null
}
