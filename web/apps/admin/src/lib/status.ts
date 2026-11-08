// 状态词与色调。与用户前台同一组状态词（docs/03 §7）；管理端多出供应商、任务、库存等内部状态。
// 主状态取服务端已排序的 blockers 第一项（docs/01 §7.2），其余阻断原因全部列出；前端不自行推断可用性。
import type {
  AccessVersion,
  AdminLifecycle,
  AdminSubscription,
  AlertKind,
  AlertSeverity,
  AlertStatus,
  Blocker,
  ExitCheck,
  ExitFeeStatus,
  ExitReason,
  ExitRequestStatus,
  FulfillmentStatus,
  LedgerSourceType,
  OrderKind,
  PaymentStatus,
  SaleStatus,
  TaskKind,
  TaskState,
  UpstreamStatus,
} from '@isp/api-client/admin'
import type { StateView } from '@isp/ui'

type Map<K extends string> = Record<K, StateView>

export const BLOCKER: Map<Blocker> = {
  account_banned: { label: '账号封禁', tone: 'stop', shape: 'hollow' },
  released: { label: '已释放', tone: 'stop', shape: 'hollow' },
  admin_paused: { label: '已暂停', tone: 'stop', shape: 'hollow' },
  pending: { label: '待履约', tone: 'wait', shape: 'half' },
  fulfill_failed: { label: '履约异常', tone: 'stop', shape: 'hollow' },
  expired: { label: '已到期', tone: 'stop', shape: 'hollow' },
  no_paid_subscription: { label: '无付费订阅', tone: 'stop', shape: 'hollow' },
  traffic_exhausted: { label: '流量耗尽', tone: 'stop', shape: 'hollow' },
  changing: { label: '变更中', tone: 'wait', shape: 'half' },
  dataplane: { label: '数据面异常', tone: 'warn', shape: 'hollow' },
}

export const USABLE: StateView = { label: '可用', tone: 'ok', shape: 'filled' }

export function primaryState(s: Pick<AdminSubscription, 'blockers' | 'lifecycle'>): StateView {
  if (s.lifecycle === 'deleted') return { label: '已删除', tone: 'stop', shape: 'hollow' }
  const first = s.blockers[0]
  return first ? BLOCKER[first] : USABLE
}

export const LIFECYCLE: Record<AdminLifecycle, string> = {
  pending: '待履约',
  fulfill_failed: '履约异常',
  active: '有效',
  retained: '保留期',
  released: '已释放',
  deleted: '已删除',
}

export const EXIT_CHECK: Map<ExitCheck> = {
  ok: { label: '检测正常', tone: 'ok', shape: 'filled' },
  slow: { label: '延迟偏高', tone: 'warn', shape: 'half' },
  failed: { label: '检测失败', tone: 'stop', shape: 'hollow' },
  stale: { label: '检测过期', tone: 'wait', shape: 'hollow' },
}

export const PAYMENT: Map<PaymentStatus> = {
  awaiting: { label: '待核款', tone: 'warn', shape: 'half' },
  confirmed: { label: '已确认', tone: 'ok', shape: 'filled' },
  cancelled: { label: '已取消', tone: 'wait', shape: 'hollow' },
  refund_review: { label: '退款核对中', tone: 'warn', shape: 'half' },
  refunded: { label: '已退款', tone: 'wait', shape: 'hollow' },
}

export const FULFILLMENT: Map<FulfillmentStatus> = {
  not_started: { label: '未开始', tone: 'wait', shape: 'hollow' },
  processing: { label: '处理中', tone: 'wait', shape: 'half' },
  delivered: { label: '已完成', tone: 'ok', shape: 'filled' },
  failed: { label: '失败待处理', tone: 'stop', shape: 'hollow' },
}

export const ORDER_KIND: Record<OrderKind, string> = {
  new: '新购',
  renew_time: '续时长',
  add_traffic: '买流量',
}

export const UPSTREAM: Map<UpstreamStatus> = {
  pending_verify: { label: '待验证', tone: 'wait', shape: 'half' },
  available: { label: '可售', tone: 'ok', shape: 'filled' },
  reserved: { label: '已预留', tone: 'wait', shape: 'half' },
  bound: { label: '已占用', tone: 'ok', shape: 'half' },
  cooldown: { label: '冷却', tone: 'wait', shape: 'hollow' },
  quarantine: { label: '异常隔离', tone: 'stop', shape: 'hollow' },
  retired: { label: '停用', tone: 'wait', shape: 'hollow' },
}

export const TASK_KIND: Record<TaskKind, string> = {
  access_change: '更换接入',
  password_reset: '重置密码',
  exit_change: '换出口',
  combined: '组合更换',
  fulfill: '开通',
  direct_grant: '赠送直出',
}

export const TASK_STATE: Map<TaskState> = {
  queued: { label: '排队中', tone: 'wait', shape: 'half' },
  running: { label: '执行中', tone: 'wait', shape: 'half' },
  succeeded: { label: '已完成', tone: 'ok', shape: 'filled' },
  failed: { label: '失败', tone: 'stop', shape: 'hollow' },
  cancelled: { label: '已取消', tone: 'wait', shape: 'hollow' },
}

export const ACCESS_STATE: Map<AccessVersion['state']> = {
  pending: { label: '待生效', tone: 'wait', shape: 'half' },
  active: { label: '生效中', tone: 'ok', shape: 'filled' },
  revoked: { label: '已撤销', tone: 'wait', shape: 'hollow' },
  failed: { label: '失败', tone: 'stop', shape: 'hollow' },
}

export const ACCESS_CHANGE: Record<AccessVersion['changeKind'], string> = {
  initial: '首次签发',
  password_reset: '重置密码',
  ports_and_password: '更换接入',
  refulfill: '重新履约',
}

export const EXIT_REQUEST: Map<ExitRequestStatus> = {
  pending: { label: '待处理', tone: 'warn', shape: 'half' },
  processing: { label: '处理中', tone: 'wait', shape: 'half' },
  succeeded: { label: '已更换', tone: 'ok', shape: 'filled' },
  failed: { label: '未成功', tone: 'stop', shape: 'hollow' },
  cancelled: { label: '已取消', tone: 'wait', shape: 'hollow' },
}

export const FEE: Record<ExitFeeStatus, string> = {
  not_applicable: '免费次数内',
  awaiting_decision: '待定收费',
  charged: '已收费',
  waived: '已免收',
}

export const EXIT_REASON: Record<ExitReason, string> = {
  target_blocked: '目标网站封禁或风控',
  quality: '速度或稳定性不佳',
  region: '想换同国家其他城市',
  other: '其他原因',
}

export const SALE: Map<SaleStatus> = {
  draft: { label: '草稿', tone: 'wait', shape: 'hollow' },
  on_sale: { label: '在售', tone: 'ok', shape: 'filled' },
  off_sale: { label: '已下架', tone: 'wait', shape: 'hollow' },
}

export const LEDGER_SOURCE: Record<LedgerSourceType, string> = {
  order: '订单',
  adjust: '调整',
  gift: '赠送',
  compensation: '补偿',
  refund: '退款回收',
  transfer_out: '划出',
  transfer_in: '划入',
}

export const SEVERITY: Map<AlertSeverity> = {
  critical: { label: '严重', tone: 'stop', shape: 'filled' },
  warning: { label: '警告', tone: 'warn', shape: 'half' },
  info: { label: '提示', tone: 'wait', shape: 'hollow' },
}

export const ALERT_STATUS: Record<AlertStatus, string> = {
  open: '未处理',
  acked: '已确认',
  resolved: '已恢复',
}

export const ALERT_KIND: Record<AlertKind, string> = {
  node_offline: '节点离线',
  upstream_error: '上游异常',
  config_failed: '配置下发失败',
  cert_risk: '证书风险',
  persist_failed: '计量持久化失败',
  clock_skew: '时钟偏差',
  fulfill_failed: '开通失败',
  exit_change_failed: '换出口失败',
  port_pool_low: '端口池不足',
  stock_low: '库存不足',
  late_payment: '晚到款',
}

export const EVENT_KIND: Record<string, string> = {
  ready: '开通就绪',
  time_added: '续时长',
  traffic_added: '买流量',
  password_reset: '重置密码',
  access_changed: '更换接入',
  exit_changed: '更换出口',
  expired: '到期',
  exhausted: '流量耗尽',
  renamed: '改名',
  paused: '暂停',
  resumed: '恢复',
  released: '释放',
  deleted: '删除',
  adjusted: '权益调整',
  transferred: '流量划转',
  refunded: '退款',
  fulfill_failed: '开通失败',
  task_failed: '任务失败',
}

export const ACTOR_KIND: Record<string, string> = { admin: '管理员', user: '用户', system: '系统', node: '节点' }

export function userState(u: { status: 'active' | 'banned'; verified: boolean }): StateView {
  if (u.status === 'banned') return { label: '已封禁', tone: 'stop', shape: 'hollow' }
  return u.verified ? { label: '正常', tone: 'ok', shape: 'filled' } : { label: '邮箱未验证', tone: 'wait', shape: 'half' }
}

export function options<K extends string>(m: Record<K, StateView | string>): { value: K; label: string }[] {
  return (Object.keys(m) as K[]).map((k) => {
    const v = m[k]
    return { value: k, label: typeof v === 'string' ? v : v.label }
  })
}
