// 订阅表格的单元格：订阅列表、用户详情、节点页共用。
import { CountryFlag, fmtBytes, fmtDate, fmtSpan } from '@isp/ui'
import type { AdminSubscription } from '@isp/api-client/admin'
import { EXIT_CHECK, primaryState } from '../lib/status'
import { BlockerChips, Mono, State, SubLink } from './bits'
import type { Column } from './DataTable'

export function SubNameCell({ s }: { s: AdminSubscription }) {
  return (
    <div className="cell-stack">
      <SubLink id={s.id} name={s.name} />
      <span className="muted">
        <Mono>{s.id}</Mono>
      </span>
    </div>
  )
}

export function SubStateCell({ s }: { s: AdminSubscription }) {
  return (
    <div className="cell-stack">
      <State view={primaryState(s)} />
      <BlockerChips blockers={s.blockers} skipFirst />
    </div>
  )
}

/** 直出代理（D-25）的标记：单色文字标签，不用国旗（没有出口国家）。 */
export function DirectTag() {
  return <span className="version-chip" title="服务器直出（管理员赠送）">直出</span>
}

export function PlanCell({ s }: { s: AdminSubscription }) {
  if (!s.plan) {
    return (
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
        <DirectTag />
        <span className="cell-stack">
          <span>服务器直出</span>
          <span className="muted">
            {s.direct?.maxConnections} 并发 · {s.direct?.bandwidthMbps} Mbps · 不限流量
          </span>
        </span>
      </span>
    )
  }
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
      <CountryFlag code={s.plan.countryCode} name={s.plan.country} size="sm" />
      <span className="cell-stack">
        <span>
          {s.plan.city} <span className="muted">v{s.plan.planVersion}</span>
        </span>
        <span className="muted">
          {s.plan.concurrency} 并发 · {s.plan.bandwidthMbps} Mbps
        </span>
      </span>
    </span>
  )
}

export function ExitCell({ s }: { s: AdminSubscription }) {
  if (s.kind === 'direct') return <span className="muted">服务器自身地址</span>
  if (!s.exit) return <span className="muted">未绑定</span>
  const ip = s.exit.verifiedIp ?? s.exit.ip
  return (
    <div className="cell-stack">
      <Mono>{ip ?? '未验证'}</Mono>
      {s.exit.check ? <State view={EXIT_CHECK[s.exit.check]} strong={false} /> : <span className="muted">未检测</span>}
    </div>
  )
}

export function TrafficCell({ s }: { s: AdminSubscription }) {
  if (s.kind === 'direct')
    return (
      <div className="cell-stack num">
        <span>不限流量</span>
        <span className="muted">累计 {fmtBytes(s.traffic.usedBytes)}</span>
      </div>
    )
  const { remainingBytes, totalBytes } = s.traffic
  const low = totalBytes > 0 && remainingBytes / totalBytes < 0.1
  return (
    <div className="cell-stack num">
      <span className={remainingBytes <= 0 ? 'tone-stop state' : low ? 'state tone-warn' : undefined}>{fmtBytes(Math.max(0, remainingBytes))}</span>
      <span className="muted">共 {fmtBytes(totalBytes)}</span>
    </div>
  )
}

export function ExpiryCell({ s }: { s: AdminSubscription }) {
  if (s.kind === 'direct') {
    const until = s.direct?.paidUntil
    return until ? (
      <div className="cell-stack num">
        <span>{fmtDate(until)}</span>
        <span className="muted">跟随付费订阅</span>
      </div>
    ) : (
      <span className="state tone-stop">无付费订阅</span>
    )
  }
  if (!s.time.expiresAt) return <span className="muted">未开始</span>
  return (
    <div className="cell-stack num">
      <span>{fmtDate(s.time.expiresAt)}</span>
      <span className="muted">{s.time.remainingSeconds > 0 ? `剩 ${fmtSpan(s.time.remainingSeconds)}` : '已到期'}</span>
    </div>
  )
}

/** 订阅表格的常用列（列表页与用户详情共用）。 */
export function subColumns(opts: { withUser?: boolean } = {}): Column<AdminSubscription>[] {
  const cols: Column<AdminSubscription>[] = [
    { key: 'name', title: '订阅', fixedVisible: true, render: (_, s) => <SubNameCell s={s} /> },
    { key: 'state', title: '状态', render: (_, s) => <SubStateCell s={s} /> },
  ]
  if (opts.withUser) cols.push({ key: 'user', title: '用户', render: (_, s) => s.user.email })
  cols.push(
    { key: 'plan', title: '套餐', render: (_, s) => <PlanCell s={s} /> },
    { key: 'exit', title: '出口', render: (_, s) => <ExitCell s={s} /> },
    { key: 'traffic', title: '剩余流量', align: 'right', render: (_, s) => <TrafficCell s={s} /> },
    { key: 'expiry', title: '到期', render: (_, s) => <ExpiryCell s={s} /> },
    {
      key: 'conns',
      title: '连接',
      align: 'right',
      render: (_, s) => (s.activeConns === null ? <span className="muted" title="节点没有上报">无数据</span> : s.activeConns),
    },
    { key: 'node', title: '节点', defaultHidden: true, render: (_, s) => (s.nodeId ? <Mono>{s.nodeId}</Mono> : <span className="muted">—</span>) },
    {
      key: 'version',
      title: '配置版本',
      defaultHidden: true,
      render: (_, s) => (
        <span className={s.desiredVersion !== s.appliedVersion ? 'state tone-warn' : undefined}>
          {s.appliedVersion} / {s.desiredVersion}
        </span>
      ),
    },
    { key: 'created', title: '创建', defaultHidden: true, render: (_, s) => fmtDate(s.createdAt) },
  )
  return cols
}
