import { ArrowLeftOutlined } from '@ant-design/icons'
import { Link } from 'react-router'
import type { DirectSubscription } from '@isp/api-client'
import { Plate } from '@isp/ui'
import { Blockers, History, Rename } from '../pages/IpDetailPage'
import { AccessPanel } from './AccessPanel'
import { DirectBalances, DirectFace, DirectMark } from './DirectCard'
import { StateCell, serial } from './SimCard'

/** 直出代理详情：卡面与用量、记录、接入（可复制连接、换接入）。没有续购、换出口入口（D-25）。 */
export function DirectDetail({ sub }: { sub: DirectSubscription }) {
  return (
    <>
      <Link className="back-link" to="/ips">
        <ArrowLeftOutlined /> 我的 IP
      </Link>
      <div className="page-head">
        <div>
          <Rename sub={sub} />
          <div className="page-head-sub">
            <span>服务器直出 · 管理员赠送</span>
            <span>
              并发 {sub.direct.maxConnections} · 带宽 {sub.direct.bandwidthMbps} Mbps · 不限流量
            </span>
          </div>
        </div>
      </div>

      <Blockers sub={sub} />

      <div className="detail">
        <div className="detail-col">
          <div className="sim-wrap">
            <section className="plate sim" aria-label="直出线路与用量" data-kind="direct">
              <div className="plate-body">
                <StateCell sub={sub} />
                <div className="sim-face">
                  <div className="sim-top">
                    <DirectMark />
                    <div className="sim-title">
                      <div className="sim-name" style={{ cursor: 'default' }}>
                        直出线路与用量
                      </div>
                      <div className="sim-plan">
                        <span className="mono" title={sub.id}>{serial(sub.id)}</span> · 跟随你的付费 IP 有效，换接入不影响
                      </div>
                    </div>
                  </div>
                  <DirectFace sub={sub} large />
                </div>
                <div className="sim-tray">
                  <DirectBalances sub={sub} detailed />
                </div>
                <div className="sim-foot">
                  <span className="foot-hint">不能续购、不能换出口：续你的付费 IP 即可延长</span>
                </div>
              </div>
            </section>
          </div>
          <History sub={sub} />
        </div>
        <div className="detail-col">
          <Plate title="直出说明" labelledBy="direct-h">
            <div className="plate-pad">
              <dl className="kv">
                <dt>出站方式</dt>
                <dd className="value">经中转服务器自身地址出站，不经 ISP 上游</dd>
                <dt>带宽上限</dt>
                <dd className="value">{sub.direct.bandwidthMbps} Mbps</dd>
                <dt>并发上限</dt>
                <dd className="value">{sub.direct.maxConnections}</dd>
                <dt>端口限制</dt>
                <dd className="value">邮件端口 25、465、587 与私网地址不可访问</dd>
              </dl>
            </div>
          </Plate>
          <AccessPanel sub={sub} />
        </div>
      </div>
    </>
  )
}
