import { RightOutlined } from '@ant-design/icons'
import { useQuery } from '@tanstack/react-query'
import { Button, Result, Skeleton } from 'antd'
import { Link } from 'react-router'
import type { Order } from '@isp/api-client'
import { orderInFlight } from '@isp/api-client'
import { POLL_MS, api, errorText, qk } from '../app/api'
import { useTitle } from '../app/Shell'
import { Plate } from '@isp/ui'
import { fmtDate, fmtMoney } from '../lib/format'
import { FULFILLMENT, PAYMENT } from '../lib/status'
import { StateMark } from '@isp/ui'

export const KIND_LABEL: Record<Order['kind'], string> = { new: '新购', renew_time: '续时长', add_traffic: '买流量' }

export function OrderChips({ o }: { o: Order }) {
  const p = PAYMENT[o.payment]
  const f = FULFILLMENT[o.fulfillment]
  return (
    <div className="chips">
      <span className={`cell tone-${p.tone}`}>
        <span className="cell-k">收款</span>
        <StateMark shape={p.shape} />
        {p.label}
      </span>
      {o.payment !== 'cancelled' && (
        <span className={`cell tone-${f.tone}`}>
          <span className="cell-k">{o.kind === 'new' ? '开通' : '入账'}</span>
          <StateMark shape={f.shape} />
          {f.label}
        </span>
      )}
      {o.payment === 'awaiting' && o.userClaimedPaidAt && <span className="plate-note">你已反馈付款，等待核对</span>}
    </div>
  )
}

export default function OrdersPage() {
  useTitle('订单')
  const q = useQuery({
    queryKey: qk.orders,
    queryFn: () => api.listOrders(),
    refetchInterval: (query) => (query.state.data?.some(orderInFlight) ? POLL_MS : false),
  })
  return (
    <>
      <div className="page-head">
        <div>
          <h1>订单</h1>
          <div className="page-head-sub">收款与开通分开显示：确认收款后才开通或入账。</div>
        </div>
      </div>
      <Plate>
        {q.isPending && (
          <div className="plate-pad">
            <Skeleton active paragraph={{ rows: 5 }} />
          </div>
        )}
        {q.isError && (
          <Result status="warning" title="订单没有加载出来" subTitle={errorText(q.error)} extra={<Button onClick={() => q.refetch()}>重新加载</Button>} />
        )}
        {q.data && q.data.length === 0 && (
          <div className="empty">
            <h2>还没有订单</h2>
            <p>
              去 <Link to="/buy">购买 IP</Link> 下第一单。
            </p>
          </div>
        )}
        {q.data && q.data.length > 0 && (
          <ul className="rows" style={{ paddingTop: 6 }}>
            {q.data.map((o) => (
              <li key={o.id}>
                <Link className="order-row" to={`/orders/${o.id}`}>
                  <span className="order-no">{o.no}</span>
                  <span className="order-what">
                    <b>
                      {KIND_LABEL[o.kind]}
                      {o.subscriptionName ? ` · ${o.subscriptionName}` : ''}
                    </b>
                    <span>
                      {o.lines.map((l) => l.detail).join('；')} · {fmtDate(o.createdAt)}
                    </span>
                  </span>
                  <span className="order-amt">{fmtMoney(o.amount)}</span>
                  <OrderChips o={o} />
                  <RightOutlined className="go" style={{ color: 'var(--ink-3)' }} />
                </Link>
              </li>
            ))}
          </ul>
        )}
      </Plate>
    </>
  )
}
