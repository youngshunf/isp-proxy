// 两端共用的时长、字节、金额与时间格式。口径：1 GB = 10⁹ 字节（D-03）；金额为分；时间统一按 UTC+08:00 展示。
// 这里只做展示换算，不做任何可用性或账务判断（以服务端字段为准）。
import dayjs from 'dayjs'
import utc from 'dayjs/plugin/utc'

dayjs.extend(utc)

/** 首期统一按 UTC+08:00 展示时间，页面写明时区。 */
const OFFSET_MIN = 480
export const TZ_LABEL = 'UTC+08:00'

export const GB = 1_000_000_000
export const MB = 1_000_000

export interface MoneyLike {
  amount: number
  currency: string
}

export function splitBytes(bytes: number): { value: string; unit: 'GB' | 'MB' } {
  if (bytes >= GB || bytes === 0) {
    const v = bytes / GB
    return { value: v >= 100 ? v.toFixed(0) : v.toFixed(1).replace(/\.0$/, ''), unit: 'GB' }
  }
  return { value: (bytes / MB).toFixed(0), unit: 'MB' }
}

export function fmtBytes(bytes: number): string {
  if (bytes < 0) return `−${fmtBytes(-bytes)}`
  const { value, unit } = splitBytes(bytes)
  return `${value} ${unit}`
}

/** 精确到两位小数的 GB 读数，账本与对账用（不会把 0.04 GB 显示成 0）。 */
export function fmtBytesFine(bytes: number): string {
  const sign = bytes < 0 ? '−' : ''
  const b = Math.abs(bytes)
  if (b >= GB) return `${sign}${(b / GB).toFixed(2)} GB`
  if (b >= MB) return `${sign}${(b / MB).toFixed(1)} MB`
  if (b >= 1000) return `${sign}${(b / 1000).toFixed(1)} KB`
  return `${sign}${b} B`
}

/** 整数字节，千分位分隔：账本原值。 */
export function fmtBytesExact(bytes: number): string {
  return `${bytes.toLocaleString('en-US')} B`
}

/** 速率：字节/秒 → 比特/秒读数。 */
export function fmtRate(bytesPerSecond: number): string {
  const bits = bytesPerSecond * 8
  if (bits >= 1e9) return `${(bits / 1e9).toFixed(2)} Gbps`
  if (bits >= 1e6) return `${(bits / 1e6).toFixed(1)} Mbps`
  if (bits >= 1e3) return `${(bits / 1e3).toFixed(0)} Kbps`
  return `${bits} bps`
}

export function splitDuration(seconds: number): { value: string; unit: string; rest?: string } {
  if (seconds <= 0) return { value: '0', unit: '天' }
  const days = Math.floor(seconds / 86_400)
  const hours = Math.floor((seconds % 86_400) / 3_600)
  if (days >= 1) return { value: String(days), unit: '天', rest: hours > 0 ? `${hours} 小时` : undefined }
  if (hours >= 1) return { value: String(hours), unit: '小时' }
  return { value: String(Math.max(1, Math.floor(seconds / 60))), unit: '分钟' }
}

/** 带符号的时长（账本的时长条目），例如 “+30 天”“−2 小时”。 */
export function fmtSpan(seconds: number, signed = false): string {
  const sign = seconds < 0 ? '−' : signed && seconds > 0 ? '+' : ''
  const s = Math.abs(seconds)
  const days = Math.floor(s / 86_400)
  const hours = Math.floor((s % 86_400) / 3_600)
  const minutes = Math.floor((s % 3_600) / 60)
  const parts: string[] = []
  if (days) parts.push(`${days} 天`)
  if (hours) parts.push(`${hours} 小时`)
  if (!days && minutes) parts.push(`${minutes} 分钟`)
  if (parts.length === 0) parts.push(`${s} 秒`)
  return sign + parts.join(' ')
}

export function fmtDate(isoStr: string | null | undefined): string {
  if (!isoStr) return '—'
  return dayjs(isoStr).utcOffset(OFFSET_MIN).format('YYYY-MM-DD HH:mm')
}

export function fmtDateTime(isoStr: string | null | undefined): string {
  if (!isoStr) return '—'
  return dayjs(isoStr).utcOffset(OFFSET_MIN).format('YYYY-MM-DD HH:mm:ss')
}

export function fmtTime(isoStr: string | null | undefined): string {
  if (!isoStr) return '—'
  return dayjs(isoStr).utcOffset(OFFSET_MIN).format('HH:mm:ss')
}

export function fmtDay(isoStr: string | null | undefined): string {
  if (!isoStr) return '—'
  return dayjs(isoStr).utcOffset(OFFSET_MIN).format('MM-DD')
}

export function addDays(isoStr: string, days: number): string {
  return dayjs(isoStr).add(days, 'day').toISOString()
}

/** “几分钟前”。now 取服务端时间（响应头 Date 的偏移），不用本机时钟判断业务。 */
export function fmtAgo(isoStr: string, now: number): string {
  const diff = Math.max(0, now - Date.parse(isoStr)) / 1000
  if (diff < 60) return '刚刚'
  if (diff < 3_600) return `${Math.floor(diff / 60)} 分钟前`
  if (diff < 86_400) return `${Math.floor(diff / 3_600)} 小时前`
  return `${Math.floor(diff / 86_400)} 天前`
}

export function fmtMoney(m: MoneyLike): string {
  return `¥${(m.amount / 100).toFixed(2)}`
}
