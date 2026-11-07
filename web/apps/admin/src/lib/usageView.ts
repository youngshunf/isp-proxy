// 用量趋势的状态判定（docs/02 A-37）：断联、空数据、零值、过期必须分开表达，不能都画成一条平线。
// - 断联：节点离线或指标请求失败，之后的空白是“不知道”，不是 0；
// - 空数据：所选时段一个样本都没有（新订阅、未开通、从未上报）；
// - 过期：有样本，但最后一次上报已超过新鲜窗口（节点在线却没上报这个订阅）；
// - 零值：有样本且流量确实为 0（空闲、暂停、耗尽后）。
// 单个时段（柱）也分三种：有流量、有样本但为 0、无样本（bytes 为 null）。
import type { UsagePoint, UsageSeries } from '@isp/api-client/admin'
import type { Tone } from '@isp/ui'

export type UsageState = 'error' | 'empty' | 'disconnected' | 'stale' | 'zero' | 'fresh'

export interface UsageView {
  state: UsageState
  label: string
  tone: Tone
  detail: string
  totalBytes: number
  /** 有样本的时段数。 */
  sampled: number
  /** 有样本但为 0 的时段数。 */
  zeroBuckets: number
  /** 无样本的时段数（在订阅开通之后、当前时间之前）。 */
  gapBuckets: number
}

export type BucketKind = 'value' | 'zero' | 'gap'

export function bucketKind(p: UsagePoint): BucketKind {
  if (p.bytes === null) return 'gap'
  return p.bytes === 0 ? 'zero' : 'value'
}

const LABEL: Record<UsageState, { label: string; tone: Tone }> = {
  error: { label: '断联', tone: 'stop' },
  disconnected: { label: '断联', tone: 'stop' },
  empty: { label: '空数据', tone: 'wait' },
  stale: { label: '数据过期', tone: 'warn' },
  zero: { label: '零值', tone: 'ok' },
  fresh: { label: '实时', tone: 'ok' },
}

export function classifyUsage(
  series: UsageSeries | undefined,
  ctx: { nodeOnline: boolean | null; error?: unknown; ago?: (iso: string) => string },
): UsageView {
  const ago = ctx.ago ?? ((s: string) => s)
  const base = { totalBytes: 0, sampled: 0, zeroBuckets: 0, gapBuckets: 0 }
  if (ctx.error || !series) {
    return { ...base, state: 'error', ...LABEL.error, detail: '没有读到指标：请求失败或指标服务不可达。图上没有数据不代表没有流量。' }
  }
  let total = 0
  let sampled = 0
  let zero = 0
  let gap = 0
  // 开头连续的无样本时段多是订阅开通前，不计作“断档”
  let started = false
  for (const p of series.points) {
    const k = bucketKind(p)
    if (k === 'gap') {
      if (started) gap++
      continue
    }
    started = true
    sampled++
    if (k === 'zero') zero++
    total += p.bytes ?? 0
  }
  // 末尾的无样本时段在断联 / 过期时属于“之后不知道”，同样计入断档
  const view = { totalBytes: total, sampled, zeroBuckets: zero, gapBuckets: gap }
  if (series.freshness === 'no_data' || sampled === 0) {
    return { ...view, state: 'empty', ...LABEL.empty, detail: '所选时段没有任何上报样本（未开通、刚开通或从未产生上报）。不是 0。' }
  }
  const last = series.lastSampleAt ? `最后样本 ${ago(series.lastSampleAt)}` : '没有最后样本时间'
  if (series.freshness === 'stale' && ctx.nodeOnline === false) {
    return { ...view, state: 'disconnected', ...LABEL.disconnected, detail: `节点离线，${last}；之后的空白是“不知道”，不是 0。` }
  }
  if (series.freshness === 'stale') {
    return { ...view, state: 'stale', ...LABEL.stale, detail: `节点在线但这个订阅的上报已过期，${last}。` }
  }
  if (total === 0) {
    return { ...view, state: 'zero', ...LABEL.zero, detail: `有上报样本，所选时段流量确实为 0（${last}）。` }
  }
  return { ...view, state: 'fresh', ...LABEL.fresh, detail: last }
}

export const RANGES = {
  '24h': { label: '近 24 小时', ms: 24 * 3600_000, bucket: '5m' as const },
  '7d': { label: '近 7 天', ms: 7 * 24 * 3600_000, bucket: '1h' as const },
  '30d': { label: '近 30 天', ms: 30 * 24 * 3600_000, bucket: '1d' as const },
}
export type RangeKey = keyof typeof RANGES

export function bucketLabel(seconds: number): string {
  if (seconds % 86_400 === 0) return `${seconds / 86_400} 天`
  if (seconds % 3600 === 0) return `${seconds / 3600} 小时`
  return `${Math.round(seconds / 60)} 分钟`
}
