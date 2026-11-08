import { ArrowRightOutlined } from '@ant-design/icons'
import { Button } from 'antd'
import { Link, useNavigate } from 'react-router'
import type { DirectSubscription } from '@isp/api-client'
import { CountryFlag } from '@isp/ui'
import { fmtBytes, fmtDate, fmtDay } from '../lib/format'
import { ProtocolCopy } from './ProtocolCopy'
import { StateCell, serial } from './SimCard'

// 服务器直出代理（D-25）的卡片：管理员赠送，经中转服务器自身地址出站，只限带宽不限流量，跟随付费 IP 有效。
// 与 SimCard 同一卡形（切角、状态格、卡面 / 卡托 / 卡脚）。直出不显示出口 IP，只显示管理员在节点上填写的
// 出口地域（如 新加坡、香港）：填了就在卡面左上角放该地域的国旗（The Flag Identity Rule：国旗代表出口所在地），
// 没填就放一枚单色的“直出”标记。没有续时长、买流量、换出口入口。

/** 卡面左上角的直出标记：与国旗同尺寸的卡托灰底单色服务器图形，不引入装饰色。 */
export function DirectMark() {
  return (
    <span className="flag flag-md direct-mark" role="img" aria-label="服务器直出" title="服务器直出">
      <svg width="20" height="18" viewBox="0 0 20 18" aria-hidden="true" focusable="false">
        <rect x="1.5" y="1.5" width="17" height="6" rx="1" fill="none" stroke="currentColor" strokeWidth="1.5" />
        <rect x="1.5" y="10.5" width="17" height="6" rx="1" fill="none" stroke="currentColor" strokeWidth="1.5" />
        <circle cx="5" cy="4.5" r="1.1" fill="currentColor" />
        <circle cx="5" cy="13.5" r="1.1" fill="currentColor" />
        <path d="M9 4.5h6M9 13.5h6" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
      </svg>
    </span>
  )
}

/** 卡面左上角：出口地域的国旗；节点还没填出口地域时用直出标记。 */
export function DirectRegionMark({ sub }: { sub: DirectSubscription }) {
  const r = sub.direct.region
  return r ? <CountryFlag code={r.countryCode} name={r.name} /> : <DirectMark />
}

/** 卡面中部：出口地域（不印出口 IP）与出站方式。 */
export function DirectFace({ sub, large = false }: { sub: DirectSubscription; large?: boolean }) {
  const r = sub.direct.region
  return (
    <div className="sim-exit">
      <div className={`direct-face${sub.blockers.length > 0 ? ' muted' : ''}`} style={large ? { fontSize: 24 } : undefined}>
        {r ? r.name : '服务器直出'}
      </div>
      <div className="exit-meta">
        <span>{r ? '服务器直出' : '经中转服务器自身地址出站'}</span>
        <span>不经 ISP 上游</span>
      </div>
    </div>
  )
}

/** 卡托：可用截止（跟随付费 IP）与累计用量（不限流量）。不画余量条：直出没有“剩余 / 总量”。 */
export function DirectBalances({ sub, detailed = false }: { sub: DirectSubscription; detailed?: boolean }) {
  const navigate = useNavigate()
  const until = sub.direct.paidUntil
  const noPaid = sub.blockers.includes('no_paid_subscription')
  const pending = sub.lifecycle === 'pending' || sub.lifecycle === 'fulfill_failed'
  return (
    <>
      <div className={`meter direct-row tone-${noPaid ? 'stop' : pending ? 'wait' : 'ok'}`}>
        <div className="meter-head">
          <span className="meter-label">可用截止</span>
          <span className="meter-read">
            {until ? (
              <>
                跟随付费 IP，至 <b>{fmtDay(until)}</b>
              </>
            ) : (
              <b>没有时长有效的付费 IP</b>
            )}
          </span>
        </div>
        {noPaid ? (
          <div className="meter-note tone-stop">
            <span>已停止转发 · 续时长或购买 IP 后自动恢复</span>
            <Button size="small" type="primary" onClick={() => navigate('/buy')}>
              购买 IP
            </Button>
          </div>
        ) : detailed && until ? (
          <div className="meter-note">
            <span className="plate-note">以你最晚到期的付费 IP 为准（{fmtDate(until)}）</span>
          </div>
        ) : null}
      </div>
      <div className="meter direct-row tone-ok">
        <div className="meter-head">
          <span className="meter-label">累计用量</span>
          <span className="meter-read">
            <b>{fmtBytes(sub.traffic.usedBytes)}</b> · 不限流量
          </span>
        </div>
        {detailed && (
          <div className="meter-note">
            <span className="plate-note">只限带宽 {sub.direct.bandwidthMbps} Mbps，用量照常计量</span>
          </div>
        )}
      </div>
    </>
  )
}

/** 我的 IP 页上的直出代理卡片。 */
export function DirectCard({ sub }: { sub: DirectSubscription }) {
  const nameId = `sim-${sub.id}`
  return (
    <div className="sim-wrap">
      <article className="plate sim" aria-labelledby={nameId} data-kind="direct">
        <div className="plate-body">
          <StateCell sub={sub} />
          <div className="sim-face">
            <div className="sim-top">
              <DirectRegionMark sub={sub} />
              <div className="sim-title">
                <Link id={nameId} className="sim-name" to={`/ips/${sub.id}`}>
                  {sub.name}
                </Link>
                <div className="sim-plan">
                  <span className="mono" title={sub.id}>{serial(sub.id)}</span> · 并发 {sub.direct.maxConnections} · {sub.direct.bandwidthMbps} Mbps
                </div>
              </div>
            </div>
            <DirectFace sub={sub} />
          </div>
          <div className="sim-tray">
            <DirectBalances sub={sub} />
          </div>
          <div className="sim-foot">
            {sub.access ? (
              <ProtocolCopy access={sub.access} dim={sub.blockers.length > 0} label={sub.name} />
            ) : (
              <span className="foot-hint">开通后在这里复制连接</span>
            )}
            <Link className="foot-link" to={`/ips/${sub.id}`}>
              详情与换接入 <ArrowRightOutlined />
            </Link>
          </div>
        </div>
      </article>
    </div>
  )
}
