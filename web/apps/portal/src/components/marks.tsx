import { useId } from 'react'
import type { MarkShape, Tone } from '../lib/status'

/** 固定格里的状态点：实心=成立，空心=不成立，半实心=处理中。形状本身携带含义，不只靠颜色。 */
export function StateMark({ shape, size = 10 }: { shape: MarkShape; size?: number }) {
  const r = 4
  return (
    <svg className="mark" width={size} height={size} viewBox="0 0 10 10" aria-hidden="true">
      {shape === 'filled' && <circle cx="5" cy="5" r={r + 0.6} fill="currentColor" />}
      {shape === 'hollow' && <circle cx="5" cy="5" r={r} fill="none" stroke="currentColor" strokeWidth="1.6" />}
      {shape === 'half' && (
        <>
          <circle cx="5" cy="5" r={r} fill="none" stroke="currentColor" strokeWidth="1.6" />
          <path d="M5 1 A4 4 0 0 0 5 9 Z" fill="currentColor" />
        </>
      )}
    </svg>
  )
}

function hash(s: string): number {
  let h = 2166136261
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619)
  return h >>> 0
}

// 四种触点分区，按订阅 ID 固定选择，让每张卡有自己的“芯片”。
const PATTERNS = [
  'M1 11.5H14M1 20.5H14M26 11.5H39M26 20.5H39M14 1V31M26 1V31M14 16H26',
  'M1 11.5H14M1 20.5H14M26 11.5H39M26 20.5H39M14 7V31M26 7V31M14 7H26M20 1V7',
  'M1 16H14M26 11.5H39M26 20.5H39M14 1V31M26 1V31M14 11H26M14 21H26',
  'M1 11.5H14M1 20.5H14M26 16H39M14 1V25M26 1V25M14 25H26M20 25V31',
]

/** SIM 卡金色触点纹样：卡面上唯一使用金色的地方。 */
export function ContactGlyph({ seed }: { seed: string }) {
  const id = useId()
  const d = PATTERNS[hash(seed) % PATTERNS.length]
  return (
    <svg width="40" height="32" viewBox="0 0 40 32" aria-hidden="true" style={{ flex: 'none' }}>
      <defs>
        <linearGradient id={id} x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="var(--gold-hi)" />
          <stop offset="0.55" stopColor="var(--gold)" />
          <stop offset="1" stopColor="#b38d1b" />
        </linearGradient>
      </defs>
      <rect x="0.5" y="0.5" width="39" height="31" rx="5" fill={`url(#${id})`} stroke="var(--gold-line)" strokeWidth="1" />
      <path d={d} fill="none" stroke="var(--gold-line)" strokeWidth="1.1" strokeLinecap="square" />
    </svg>
  )
}

const TONE_COLOR: Record<Tone, string> = {
  ok: 'var(--teal)',
  warn: 'var(--warn-fill)',
  stop: 'var(--stop)',
  wait: 'var(--ink-3)',
}

/** 出口健康的信号格。 */
export function SignalBars({ bars, tone, outline = false }: { bars: number; tone: Tone; outline?: boolean }) {
  const heights = [4, 7, 10, 13]
  return (
    <svg width="18" height="14" viewBox="0 0 18 14" aria-hidden="true">
      {heights.map((h, i) => {
        const on = i < bars
        return (
          <rect
            key={h}
            x={i * 4.6 + 0.5}
            y={14 - h}
            width="3.2"
            height={h}
            fill={outline ? 'none' : on ? TONE_COLOR[tone] : '#cfd8d4'}
            stroke={outline ? '#9aa8a3' : 'none'}
            strokeWidth={outline ? 1 : 0}
            strokeDasharray={outline ? '1.5 1.2' : undefined}
          />
        )
      })}
    </svg>
  )
}
