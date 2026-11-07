// 状态文案与补救动作。主状态取阻断原因的第一项（服务端已按 docs/01 §7.2 排序），
// 其余阻断原因全部列出；前端不自行推断可用性。
import {
  remainingBytes,
  type Blocker,
  type ExitCheck,
  type ExitRequestStatus,
  type FulfillmentStatus,
  type PaymentStatus,
  type Subscription,
} from '@isp/api-client'
import { fmtDay } from './format'

export type Tone = 'ok' | 'warn' | 'stop' | 'wait'
export type MarkShape = 'filled' | 'hollow' | 'half'

export interface StateView {
  label: string
  tone: Tone
  shape: MarkShape
}

const BLOCKER_STATE: Record<Blocker, StateView> = {
  account_banned: { label: '账号已封禁', tone: 'stop', shape: 'hollow' },
  released: { label: '已释放', tone: 'stop', shape: 'hollow' },
  admin_paused: { label: '已暂停', tone: 'stop', shape: 'hollow' },
  pending: { label: '开通中', tone: 'wait', shape: 'half' },
  fulfill_failed: { label: '开通异常', tone: 'stop', shape: 'hollow' },
  expired: { label: '已到期', tone: 'stop', shape: 'hollow' },
  traffic_exhausted: { label: '流量用尽', tone: 'stop', shape: 'hollow' },
  changing: { label: '变更中', tone: 'wait', shape: 'half' },
  dataplane: { label: '线路异常', tone: 'stop', shape: 'hollow' },
}

export const LOW_TRAFFIC_RATIO = 0.1
export const EXPIRING_SECONDS = 3 * 86_400

export function primaryState(sub: Subscription): StateView {
  const first = sub.blockers[0]
  if (first) return BLOCKER_STATE[first]
  return { label: '可用', tone: 'ok', shape: 'filled' }
}

export function isExpiring(sub: Subscription): boolean {
  return sub.blockers.length === 0 && sub.time.remainingSeconds > 0 && sub.time.remainingSeconds <= EXPIRING_SECONDS
}

export function isLowTraffic(sub: Subscription): boolean {
  const left = remainingBytes(sub)
  return left > 0 && sub.traffic.totalBytes > 0 && left / sub.traffic.totalBytes < LOW_TRAFFIC_RATIO
}

export function blockerLabel(b: Blocker): string {
  return BLOCKER_STATE[b].label
}

/** 卡片和详情页上的阻断说明：讲清原因、影响和恢复办法。 */
export function blockerText(b: Blocker, sub: Subscription): string {
  switch (b) {
    case 'expired':
      return sub.time.retentionEndsAt
        ? `已于 ${fmtDay(sub.time.expiresAt)} 到期，停止转发。原出口与接入保留到 ${fmtDay(sub.time.retentionEndsAt)}，期内续时长即可恢复；剩余流量保留。`
        : '时长已到期，停止转发；剩余流量保留，续时长后重新开通。'
    case 'traffic_exhausted':
      return '流量已用尽，全部连接已断开。买流量后恢复，到期时间不变。'
    case 'pending':
      return '已确认收款，正在分配出口并签发接入。开通就绪后才开始计时。'
    case 'fulfill_failed':
      return '开通遇到问题，管理员正在处理；计时尚未开始。'
    case 'admin_paused':
      return '管理员已暂停这个 IP。续购不能解除暂停，请联系客服。'
    case 'account_banned':
      return '账号已被封禁，全部 IP 停止转发。'
    case 'released':
      return '保留期已满，出口与接入已释放；剩余流量仍在。续时长即重新开通，会分配新出口和新接入。'
    case 'changing':
      return '正在切换接入或出口，完成后自动恢复。'
    case 'dataplane':
      return '出口检测失败，平台正在排查；恢复后自动可用，无需操作。'
  }
}

export interface Remedy {
  label: string
  to: string
}

export function remedyFor(b: Blocker, sub: Subscription): Remedy | null {
  switch (b) {
    case 'expired':
    case 'released':
      return { label: '续时长', to: `/buy/renew/${sub.id}?kind=time` }
    case 'traffic_exhausted':
      return { label: '买流量', to: `/buy/renew/${sub.id}?kind=traffic` }
    case 'pending':
    case 'fulfill_failed': {
      const order = sub.openOrderIds[0]
      return order ? { label: '查看订单', to: `/orders/${order}` } : null
    }
    default:
      return null
  }
}

export const EXIT_CHECK: Record<ExitCheck, { label: string; tone: Tone; bars: number }> = {
  ok: { label: '检测正常', tone: 'ok', bars: 4 },
  slow: { label: '延迟偏高', tone: 'warn', bars: 2 },
  failed: { label: '检测失败', tone: 'stop', bars: 0 },
  stale: { label: '检测已过期', tone: 'wait', bars: 0 },
}

export const PAYMENT: Record<PaymentStatus, { label: string; tone: Tone; shape: MarkShape }> = {
  awaiting: { label: '待核款', tone: 'warn', shape: 'half' },
  confirmed: { label: '已确认', tone: 'ok', shape: 'filled' },
  cancelled: { label: '已取消', tone: 'wait', shape: 'hollow' },
  refund_review: { label: '退款核对中', tone: 'warn', shape: 'half' },
  refunded: { label: '已退款', tone: 'wait', shape: 'hollow' },
}

export const FULFILLMENT: Record<FulfillmentStatus, { label: string; tone: Tone; shape: MarkShape }> = {
  not_started: { label: '未开始', tone: 'wait', shape: 'hollow' },
  processing: { label: '处理中', tone: 'wait', shape: 'half' },
  delivered: { label: '已完成', tone: 'ok', shape: 'filled' },
  failed: { label: '失败待处理', tone: 'stop', shape: 'hollow' },
}

export const EXIT_REQUEST: Record<ExitRequestStatus, { label: string; tone: Tone; shape: MarkShape }> = {
  pending: { label: '待处理', tone: 'wait', shape: 'hollow' },
  processing: { label: '处理中', tone: 'wait', shape: 'half' },
  succeeded: { label: '已更换', tone: 'ok', shape: 'filled' },
  failed: { label: '未成功', tone: 'stop', shape: 'hollow' },
  cancelled: { label: '已取消', tone: 'wait', shape: 'hollow' },
}

export const EXIT_REASON = {
  target_blocked: '目标网站封禁或风控',
  quality: '速度或稳定性不佳',
  region: '想换同国家其他城市',
  other: '其他原因',
} as const
