import { ClockCircleOutlined, ExclamationCircleOutlined, PlusOutlined, WalletOutlined } from '@ant-design/icons'
import { useQuery } from '@tanstack/react-query'
import { Button, Result, Skeleton } from 'antd'
import type { ReactNode } from 'react'
import { Link, useNavigate } from 'react-router'
import { remainingBytes, type Subscription } from '@isp/api-client'
import { api, errorText, qk } from '../app/api'
import { StateMark } from '../components/marks'
import { Plate } from '../components/Plate'
import { SimCard } from '../components/SimCard'
import { fmtBytes, splitDuration } from '../lib/format'
import { isExpiring, isLowTraffic } from '../lib/status'

function Counts({ subs }: { subs: Subscription[] }) {
  const usable = subs.filter((s) => s.blockers.length === 0).length
  const pending = subs.filter((s) => s.blockers[0] === 'pending' || s.blockers[0] === 'changing').length
  const stopped = subs.length - usable - pending
  return (
    <div className="page-head-sub">
      <span className="count">
        共 <b>{subs.length}</b> 个
      </span>
      <span className="count" style={{ color: 'var(--ok)' }}>
        <StateMark shape="filled" />
        <span style={{ color: 'var(--ink-2)' }}>
          可用 <b>{usable}</b>
        </span>
      </span>
      <span className="count" style={{ color: 'var(--stop)' }}>
        <StateMark shape="hollow" />
        <span style={{ color: 'var(--ink-2)' }}>
          停用 <b>{stopped}</b>
        </span>
      </span>
      <span className="count" style={{ color: 'var(--teal)' }}>
        <StateMark shape="half" />
        <span style={{ color: 'var(--ink-2)' }}>
          开通中 <b>{pending}</b>
        </span>
      </span>
    </div>
  )
}

/** 概览并入此处：即将到期、低流量、待核款订单。 */
function Attention({ subs, awaiting }: { subs: Subscription[]; awaiting: number }) {
  const items: { key: string; to: string; icon: ReactNode; text: string }[] = []
  for (const s of subs) {
    if (isExpiring(s)) {
      const d = splitDuration(s.time.remainingSeconds)
      items.push({
        key: `${s.id}-t`,
        to: `/buy/renew/${s.id}?kind=time`,
        icon: <ClockCircleOutlined />,
        text: `${s.name} ${d.value} ${d.unit}后到期`,
      })
    }
    if (isLowTraffic(s)) {
      items.push({
        key: `${s.id}-b`,
        to: `/buy/renew/${s.id}?kind=traffic`,
        icon: <ExclamationCircleOutlined />,
        text: `${s.name} 仅剩 ${fmtBytes(remainingBytes(s))} 流量`,
      })
    }
  }
  if (awaiting > 0) {
    items.push({ key: 'orders', to: '/orders', icon: <WalletOutlined />, text: `${awaiting} 个订单等待人工核款` })
  }
  if (items.length === 0) return null
  return (
    <div className="attention" role="region" aria-label="需要留意">
      {items.map((i) => (
        <Link key={i.key} className="attention-item" to={i.to}>
          {i.icon}
          <span>{i.text}</span>
        </Link>
      ))}
    </div>
  )
}

export default function IpsPage() {
  const navigate = useNavigate()
  const subs = useQuery({ queryKey: qk.subs, queryFn: () => api.listSubscriptions() })
  const orders = useQuery({ queryKey: qk.orders, queryFn: () => api.listOrders() })
  const awaiting = orders.data?.filter((o) => o.payment === 'awaiting').length ?? 0

  return (
    <>
      <div className="page-head">
        <div>
          <h1>我的 IP</h1>
          {subs.data ? <Counts subs={subs.data} /> : <div className="page-head-sub">正在读取…</div>}
        </div>
        <Button type="primary" size="large" icon={<PlusOutlined />} onClick={() => navigate('/buy')}>
          购买 IP
        </Button>
      </div>

      {subs.isPending && (
        <div className="sim-grid" aria-busy="true">
          {[0, 1, 2].map((i) => (
            <Plate key={i}>
              <div className="plate-pad">
                <Skeleton active paragraph={{ rows: 6 }} />
              </div>
            </Plate>
          ))}
        </div>
      )}

      {subs.isError && (
        <Plate>
          <Result
            status="warning"
            title="IP 列表没有加载出来"
            subTitle={errorText(subs.error)}
            extra={<Button onClick={() => subs.refetch()}>重新加载</Button>}
          />
        </Plate>
      )}

      {subs.data && subs.data.length === 0 && (
        <Plate>
          <div className="empty">
            <h2>还没有 IP</h2>
            <p>选一个地区下单，管理员核款后开通，开通后在这里复制连接。</p>
            <Button type="primary" onClick={() => navigate('/buy')}>
              去选购
            </Button>
          </div>
        </Plate>
      )}

      {subs.data && subs.data.length > 0 && (
        <>
          <Attention subs={subs.data} awaiting={awaiting} />
          <div className="sim-grid">
            {subs.data.map((s) => (
              <SimCard key={s.id} sub={s} />
            ))}
          </div>
        </>
      )}
    </>
  )
}
