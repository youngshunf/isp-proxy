import type { ReactNode } from 'react'
import type { Tone } from './tone'

/** “剩余 / 总量”横条读数。条长表示剩余，不表示已用。 */
export function Meter({
  label,
  value,
  unit,
  total,
  ratio,
  tone = 'ok',
  rest,
  note,
}: {
  label: string
  value: string
  unit: string
  total?: string
  ratio: number
  tone?: Tone
  rest?: string
  note?: ReactNode
}) {
  const pct = Math.max(0, Math.min(1, ratio))
  const readable = `${label}：${value} ${unit}${rest ? ` ${rest}` : ''}${total ? `，共 ${total}` : ''}`
  return (
    <div className={`meter tone-${tone}`}>
      <div className="meter-head">
        <span className="meter-label">{label}</span>
        <span className="meter-read" aria-hidden="true">
          <b>{value}</b> {unit}
          {rest ? ` ${rest}` : ''}
          {total ? ` / ${total}` : ''}
        </span>
      </div>
      <div
        className="meter-track"
        role="meter"
        aria-label={readable}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(pct * 100)}
      >
        <div className="meter-fill" style={{ width: `${pct * 100}%` }} />
      </div>
      {note}
    </div>
  )
}
