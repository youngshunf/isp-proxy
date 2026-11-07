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
