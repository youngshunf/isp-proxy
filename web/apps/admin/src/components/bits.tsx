// 管理后台的小部件：状态文字、阻断原因、等宽串、页头、键值表、加载/失败占位。
import { ArrowLeftOutlined, CopyOutlined, ReloadOutlined } from '@ant-design/icons'
import { App, Button, Result, Skeleton, Tooltip } from 'antd'
import type { ReactNode } from 'react'
import { Link } from 'react-router'
import type { Blocker } from '@isp/api-client/admin'
import { StateMark, fmtAgo, fmtDateTime, type StateView } from '@isp/ui'
import { errorText, useServerNow } from '../app/api'
import { BLOCKER } from '../lib/status'

/** 形状圆点 + 文字，颜色按 tone。状态永远不只靠颜色表达。 */
export function State({ view, strong = true }: { view: StateView; strong?: boolean }) {
  return (
    <span className={`state tone-${view.tone}${strong ? ' strong' : ''}`}>
      <StateMark shape={view.shape} />
      {view.label}
    </span>
  )
}

/** 带键名的状态格（收款 / 履约等两个维度分开表达）。 */
export function Cell({ k, view }: { k: string; view: StateView }) {
  return (
    <span className={`cell tone-${view.tone}`}>
      <span className="cell-k">{k}</span>
      <StateMark shape={view.shape} />
      {view.label}
    </span>
  )
}

export function BlockerChips({ blockers, skipFirst = false }: { blockers: Blocker[]; skipFirst?: boolean }) {
  const list = skipFirst ? blockers.slice(1) : blockers
  if (list.length === 0) return null
  return (
    <span className="chips">
      {list.map((b) => (
        <span key={b} className={`chip tone-${BLOCKER[b].tone}`}>
          {BLOCKER[b].label}
        </span>
      ))}
    </span>
  )
}

export function Mono({ children, className = '' }: { children: ReactNode; className?: string }) {
  return <span className={`mono ${className}`}>{children}</span>
}

export function Ago({ at, empty = '—' }: { at: string | null | undefined; empty?: string }) {
  const now = useServerNow()
  if (!at) return <span className="muted">{empty}</span>
  return (
    <time dateTime={at} title={fmtDateTime(at)}>
      {fmtAgo(at, now())}
    </time>
  )
}

export function CopyButton({ text, label = '复制' }: { text: string; label?: string }) {
  const { message } = App.useApp()
  return (
    <Tooltip title={label}>
      <Button
        type="text"
        size="small"
        icon={<CopyOutlined />}
        aria-label={`${label} ${text}`}
        onClick={async () => {
          try {
            await navigator.clipboard.writeText(text)
            message.success('已复制')
          } catch {
            message.error('复制失败，请手动选择')
          }
        }}
      />
    </Tooltip>
  )
}

export function PageHead({
  title,
  sub,
  extra,
  back,
}: {
  title: ReactNode
  sub?: ReactNode
  extra?: ReactNode
  back?: { to: string; label: string }
}) {
  return (
    <div className="page-head">
      <div className="page-head-main">
        {back && (
          <Link to={back.to} className="back-link">
            <ArrowLeftOutlined /> {back.label}
          </Link>
        )}
        <h1>{title}</h1>
        {sub && <div className="page-head-sub">{sub}</div>}
      </div>
      {extra && <div className="page-head-extra">{extra}</div>}
    </div>
  )
}

/** 键值表：键名 112px 灰字，值可换行。 */
export function Facts({ items, cols = 1 }: { items: [ReactNode, ReactNode][]; cols?: 1 | 2 }) {
  return (
    <dl className={`facts cols-${cols}`}>
      {items.map(([k, v], i) => (
        <div key={i} className="fact">
          <dt>{k}</dt>
          <dd>{v ?? <span className="muted">—</span>}</dd>
        </div>
      ))}
    </dl>
  )
}

export function Loading({ rows = 4 }: { rows?: number }) {
  return (
    <div className="plate-pad" aria-busy="true">
      <Skeleton active title={false} paragraph={{ rows }} />
    </div>
  )
}

export function LoadError({ error, retry }: { error: unknown; retry?: () => void }) {
  return (
    <Result
      status="warning"
      title="没有加载出来"
      subTitle={errorText(error)}
      extra={
        retry ? (
          <Button icon={<ReloadOutlined />} onClick={retry}>
            重试
          </Button>
        ) : null
      }
    />
  )
}

export function Callout({ tone = 'info', icon, children }: { tone?: 'info' | 'warn' | 'stop'; icon?: ReactNode; children: ReactNode }) {
  return (
    <div className={`callout ${tone}`} role={tone === 'stop' ? 'alert' : undefined}>
      {icon}
      <div>{children}</div>
    </div>
  )
}

export function SubLink({ id, name }: { id: string; name?: string | null }) {
  return (
    <Link to={`/subscriptions/${id}`} className="sub-link">
      {name ?? <Mono>{id}</Mono>}
    </Link>
  )
}
