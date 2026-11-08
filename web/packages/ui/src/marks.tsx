import type { MarkShape, Tone } from './tone'

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

// 国旗来自 flag-icons（MIT），4:3 比例；按需取 URL，只有出现在页面上的国旗才会下载。
const FLAGS = import.meta.glob<string>('../node_modules/flag-icons/flags/4x3/*.svg', {
  query: '?url',
  import: 'default',
  eager: true,
})

function flagUrl(code: string): string | undefined {
  return FLAGS[`../node_modules/flag-icons/flags/4x3/${code.toLowerCase()}.svg`]
}

/** 出口所在国家的国旗：卡面左上角的识别标记。未知国家显示国家代码。 */
export function CountryFlag({ code, name, size = 'md' }: { code: string; name: string; size?: 'sm' | 'md' }) {
  const url = flagUrl(code)
  return (
    <span className={`flag flag-${size}`} role="img" aria-label={name} title={name}>
      {url ? <img src={url} alt="" draggable={false} /> : <span className="flag-code">{code.toUpperCase()}</span>}
    </span>
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

/** 品牌色：只用于品牌标记，不进入界面配色（界面强调色仍是青绿 --teal）。 */
export const BRAND_COLOR = '#F26A10'

/**
 * 驿卡品牌标记：切角 SIM 卡轮廓里，入口与出口两个站点由一段驿路相连。
 * 与 public/favicon.svg 同一几何；颜色固定，不随主题变化。
 */
export function BrandMark({ size = 28 }: { size?: number }) {
  return (
    <svg className="brand-mark" width={size} height={size} viewBox="0 0 64 64" aria-hidden="true" focusable="false">
      <path d="M13 3H39L51 15V61H13Z" fill={BRAND_COLOR} />
      <path d="M24 49V37H40V25" fill="none" stroke="#fff" strokeWidth="3.5" />
      <circle cx="24" cy="49" r="4.5" fill="#fff" />
      <circle cx="40" cy="25" r="4.5" fill="#fff" />
    </svg>
  )
}

/** 出口类型（D-24）的文字。 */
export const EXIT_MODE_LABEL = { exclusive: '独享', shared: '共享' } as const

/**
 * 出口类型标签片：独享为普通描边片，共享用深青淡底；文字本身携带含义，不只靠颜色。
 * 共享时 capacity 给出“最多 N 人共用”的上限（用户端只显示上限，不显示当前人数）。
 */
export function ExitModeChip({ mode, capacity }: { mode: 'exclusive' | 'shared'; capacity?: number | null }) {
  const text = mode === 'shared' && capacity ? `共享 · 最多 ${capacity} 人` : EXIT_MODE_LABEL[mode]
  return <span className={`chip exit-mode${mode === 'shared' ? ' tone-wait' : ''}`}>{text}</span>
}
