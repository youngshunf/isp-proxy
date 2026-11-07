import dayjs from 'dayjs'
import utc from 'dayjs/plugin/utc'
import type { Money } from '@isp/api-client'
import { serverNow } from '../app/api'

dayjs.extend(utc)

/** 首期统一按 UTC+08:00 展示时间，账号页与页脚都写明时区。 */
const OFFSET_MIN = 480
export const TZ_LABEL = 'UTC+08:00'

const GB = 1_000_000_000
const MB = 1_000_000

export function splitBytes(bytes: number): { value: string; unit: 'GB' | 'MB' } {
  if (bytes >= GB || bytes === 0) {
    const v = bytes / GB
    return { value: v >= 100 ? v.toFixed(0) : v.toFixed(1).replace(/\.0$/, ''), unit: 'GB' }
  }
  return { value: (bytes / MB).toFixed(0), unit: 'MB' }
}

export function fmtBytes(bytes: number): string {
  const { value, unit } = splitBytes(bytes)
  return `${value} ${unit}`
}

export function splitDuration(seconds: number): { value: string; unit: string; rest?: string } {
  if (seconds <= 0) return { value: '0', unit: '天' }
  const days = Math.floor(seconds / 86_400)
  const hours = Math.floor((seconds % 86_400) / 3_600)
  if (days >= 1) return { value: String(days), unit: '天', rest: hours > 0 ? `${hours} 小时` : undefined }
  if (hours >= 1) return { value: String(hours), unit: '小时' }
  return { value: String(Math.max(1, Math.floor(seconds / 60))), unit: '分钟' }
}

export function fmtDate(isoStr: string | null | undefined): string {
  if (!isoStr) return '—'
  return dayjs(isoStr).utcOffset(OFFSET_MIN).format('YYYY-MM-DD HH:mm')
}

export function fmtDay(isoStr: string | null | undefined): string {
  if (!isoStr) return '—'
  return dayjs(isoStr).utcOffset(OFFSET_MIN).format('MM-DD')
}

export function addDays(isoStr: string, days: number): string {
  return dayjs(isoStr).add(days, 'day').toISOString()
}

export function fmtAgo(isoStr: string): string {
  const diff = Math.max(0, serverNow() - Date.parse(isoStr)) / 1000
  if (diff < 60) return '刚刚'
  if (diff < 3_600) return `${Math.floor(diff / 60)} 分钟前`
  if (diff < 86_400) return `${Math.floor(diff / 3_600)} 小时前`
  return `${Math.floor(diff / 86_400)} 天前`
}

export function fmtMoney(m: Money): string {
  return `¥${(m.amount / 100).toFixed(2)}`
}
