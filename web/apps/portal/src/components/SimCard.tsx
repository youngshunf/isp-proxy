import { ArrowRightOutlined } from '@ant-design/icons'
import { Link } from 'react-router'
import type { Subscription } from '@isp/api-client'
import { fmtAgo } from '../lib/format'
import { EXIT_CHECK, primaryState } from '../lib/status'
import { Balances } from './Balances'
import { CountryFlag, SignalBars, StateMark } from '@isp/ui'
import { ProtocolCopy } from './ProtocolCopy'

/** 卡面印字：订阅编号，帮助客服与用户对同一张卡。 */
export function serial(id: string): string {
  return id.replace(/^sub_/, 'SUB-').toUpperCase()
}

export function StateCell({ sub }: { sub: Subscription }) {
  const st = primaryState(sub)
  return (
    <div className={`state-cell tone-${st.tone}`}>
      <StateMark shape={st.shape} />
      <span>{st.label}</span>
    </div>
  )
}

export function ExitFace({ sub, large = false }: { sub: Subscription; large?: boolean }) {
  const blocked = sub.blockers.length > 0
  if (!sub.exit) {
    return (
      <div className="sim-exit">
        <div className="exit-ip placeholder">出口开通中</div>
        <div className="exit-meta">
          <span>
            {sub.plan.country} · {sub.plan.city}
          </span>
          <span>具体地址在开通时分配</span>
        </div>
      </div>
    )
  }
  const chk = EXIT_CHECK[sub.exit.check]
  return (
    <div className="sim-exit">
      <div className="exit-ip-row">
        <div className={`exit-ip${blocked ? ' muted' : ''}`} style={large ? { fontSize: 32 } : undefined}>
          {sub.exit.ip}
        </div>
        <span className="signal" title={chk.label}>
          <SignalBars bars={chk.bars} tone={chk.tone} outline={sub.exit.check === 'stale'} />
          {sub.exit.latencyMs !== null && sub.exit.check !== 'stale' ? `${sub.exit.latencyMs} ms` : chk.label}
        </span>
      </div>
      <div className="exit-meta">
        <span>
          {sub.exit.country} · {sub.exit.city}
        </span>
        <span>
          {sub.exit.check === 'stale' ? '上次检测' : '实测'} {fmtAgo(sub.exit.checkedAt)}
        </span>
      </div>
    </div>
  )
}

/** 我的 IP 页上的一张 SIM 卡：卡面是出口，卡托是套餐余量，卡脚是复制连接。 */
export function SimCard({ sub }: { sub: Subscription }) {
  const nameId = `sim-${sub.id}`
  const extraBlockers = sub.blockers.slice(1)
  return (
    <div className="sim-wrap">
      <article className="plate sim" aria-labelledby={nameId}>
        <div className="plate-body">
          <StateCell sub={sub} />
          <div className="sim-face">
            <div className="sim-top">
              <CountryFlag code={sub.exit?.countryCode ?? sub.plan.countryCode} name={sub.exit?.country ?? sub.plan.country} />
              <div className="sim-title">
                <Link id={nameId} className="sim-name" to={`/ips/${sub.id}`}>
                  {sub.name}
                </Link>
                <div className="sim-plan">
                  <span className="mono">{serial(sub.id)}</span> · 并发 {sub.plan.concurrency} · {sub.plan.bandwidthMbps} Mbps
                </div>
              </div>
            </div>
            <ExitFace sub={sub} />
          </div>
          <div className="sim-tray">
            <Balances sub={sub} />
            {extraBlockers.length > 0 && (
              <div className="meter-note tone-stop">同时存在：{extraBlockers.length} 项其他阻断，详见详情</div>
            )}
            {sub.openExitRequestId && (
              <div className="meter-note tone-wait">
                <span>换出口申请处理中，当前出口照常可用</span>
                <Link to="/requests">查看进度</Link>
              </div>
            )}
            {sub.lifecycle !== 'pending' && sub.openOrderIds.length > 0 && (
              <div className="meter-note tone-wait">
                <span>有 {sub.openOrderIds.length} 个续购订单待核款</span>
                <Link to={`/orders/${sub.openOrderIds[0]}`}>查看订单</Link>
              </div>
            )}
          </div>
          <div className="sim-foot">
            {sub.access ? (
              <ProtocolCopy access={sub.access} dim={sub.blockers.length > 0} label={sub.name} />
            ) : (
              <span className="foot-hint">开通后在这里复制连接</span>
            )}
            <Link className="foot-link" to={sub.access ? `/ips/${sub.id}` : `/orders/${sub.openOrderIds[0] ?? ''}`}>
              {sub.access ? '详情与更换' : '查看订单'} <ArrowRightOutlined />
            </Link>
          </div>
        </div>
      </article>
    </div>
  )
}
