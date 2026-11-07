// 运营概览（R-12）：待核款订单置顶，其次是告警与各类计数。所有计数可点进对应筛选后的列表。
// 流量类指标来自节点上报：没有数据时写“无数据”，不写 0（A-37）。
import { AlertOutlined, ClockCircleOutlined, DollarOutlined } from '@ant-design/icons'
import { useQuery } from '@tanstack/react-query'
import { Button } from 'antd'
import { useState } from 'react'
import { Link } from 'react-router'
import type { AdminOrder } from '@isp/api-client/admin'
import { Plate, StateMark, fmtBytes, fmtDateTime, fmtMoney, fmtRate } from '@isp/ui'
import { qk, useApi } from '../app/api'
import { Ago, LoadError, Loading, Mono, PageHead, State } from '../components/bits'
import { ConfirmPaymentDialog } from '../components/ConfirmPaymentDialog'
import { ALERT_KIND, ORDER_KIND, SEVERITY } from '../lib/status'

export default function OverviewPage() {
  const api = useApi()
  const ov = useQuery({ queryKey: qk.overview, queryFn: () => api.adminGetOverview(), refetchInterval: 30_000 })
  const awaiting = useQuery({
    queryKey: [...qk.orders, 'awaiting-pinned'],
    queryFn: () => api.adminListOrders({ query: { payment: ['awaiting'], limit: 20 } }),
    refetchInterval: 30_000,
  })
  const alerts = useQuery({
    queryKey: [...qk.alerts, 'open-pinned'],
    queryFn: () => api.adminListAlerts({ query: { status: ['open'], limit: 6 } }),
  })
  const [paying, setPaying] = useState<AdminOrder | null>(null)

  if (ov.isPending) return <Loading rows={8} />
  if (ov.isError) return <LoadError error={ov.error} retry={() => void ov.refetch()} />
  const o = ov.data
  const s = o.subscriptions

  // 用户已标记付款的排在前面，其次按创建时间
  const orders = [...(awaiting.data?.items ?? [])].sort(
    (a, b) => Number(!!b.userClaimedPaidAt) - Number(!!a.userClaimedPaidAt) || a.createdAt.localeCompare(b.createdAt),
  )

  return (
    <>
      <PageHead
        title="运营概览"
        sub={
          <>
            <span>
              生成于 <Ago at={o.generatedAt} />
            </span>
            <span>
              节点指标 <Ago at={o.dataAt} empty="无上报" />
            </span>
          </>
        }
      />
      <div className="stack">
        <Plate
          className="pinned-wrap"
          title={
            <div className="pinned-head">
              <DollarOutlined />
              <h2>待核款订单</h2>
              <span className="count">{o.orders.awaitingPayment}</span>
            </div>
          }
          extra={
            <Link to="/orders?payment=awaiting" className="small">
              全部待核款
            </Link>
          }
        >
          <div className="plate-pad" style={{ paddingTop: 6, paddingBottom: 0 }}>
            <p className="muted small">核对银行流水的金额与付款备注码后再确认；同一订单只会记一次收款。</p>
          </div>
          {awaiting.isPending ? (
            <Loading rows={3} />
          ) : orders.length === 0 ? (
            <div className="empty-block">没有待核款订单。</div>
          ) : (
            <ul className="rows" aria-label="待核款订单">
              {orders.map((x) => (
                <li key={x.id}>
                  <div className="row-main">
                    <div className="row-title">
                      <Link to={`/orders/${x.id}`}>
                        <Mono>{x.no}</Mono>
                      </Link>
                      <span>{ORDER_KIND[x.kind]}</span>
                      {x.subscriptionName && <span className="muted">· {x.subscriptionName}</span>}
                      {x.userClaimedPaidAt ? (
                        <State view={{ label: '用户已标记付款', tone: 'warn', shape: 'half' }} />
                      ) : (
                        <span className="muted small">用户未标记付款</span>
                      )}
                    </div>
                    <div className="row-meta">
                      <span>{x.user.email}</span>
                      <span>
                        备注码 <Mono>{x.payRef}</Mono>
                      </span>
                      <span>下单 {fmtDateTime(x.createdAt)}</span>
                      {x.holdUntil && (
                        <span>
                          <ClockCircleOutlined /> 保留至 {fmtDateTime(x.holdUntil)}
                        </span>
                      )}
                    </div>
                  </div>
                  <div className="row-side">
                    <span className="amount">{fmtMoney(x.amount)}</span>
                    <Button type="primary" size="small" onClick={() => setPaying(x)}>
                      确认收款
                    </Button>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </Plate>

        <div className="grid-2">
          <Plate
            title={
              <div className="pinned-head">
                <AlertOutlined />
                <h2>未处理告警</h2>
                <span className={`count${o.alerts.critical ? ' tone-stop' : ''}`}>{o.alerts.open}</span>
              </div>
            }
            extra={
              <Link to="/alerts" className="small">
                告警列表
              </Link>
            }
          >
            {alerts.isPending ? (
              <Loading rows={3} />
            ) : (alerts.data?.items.length ?? 0) === 0 ? (
              <div className="empty-block">没有未处理的告警。</div>
            ) : (
              <ul className="rows" style={{ marginTop: 12 }}>
                {alerts.data!.items.map((a) => (
                  <li key={a.id}>
                    <div className="row-main">
                      <div className="row-title">
                        <State view={SEVERITY[a.severity]} />
                        <span>{ALERT_KIND[a.kind]}</span>
                      </div>
                      <div className="row-meta">
                        <span>{a.message}</span>
                      </div>
                    </div>
                    <div className="row-side muted small">
                      <Ago at={a.lastSeenAt} />
                      {a.occurrences > 1 && <span>×{a.occurrences}</span>}
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </Plate>

          <Plate title="流量与节点" extra={<Link to="/monitoring" className="small">节点与监控</Link>}>
            <div className="stats" style={{ marginTop: 12 }}>
              <Stat k="当前连接" v={o.traffic.currentConns === null ? null : String(o.traffic.currentConns)} note="节点上报汇总" />
              <Stat k="当前速率" v={o.traffic.rateBps === null ? null : fmtRate(o.traffic.rateBps)} />
              <Stat k="今日流量" v={fmtBytes(o.traffic.todayBytes)} note="按账本已计量" />
              <Stat k="累计流量" v={fmtBytes(o.traffic.totalBytes)} />
              <Stat k="节点在线" v={String(o.nodes.online)} to="/monitoring" />
              <Stat k="节点离线" v={String(o.nodes.offline)} tone={o.nodes.offline ? 'stop' : undefined} to="/monitoring" />
            </div>
            <p className="plate-pad muted small" style={{ paddingTop: 10 }}>
              “无数据”表示节点没有上报，不等于 0。数据时间 {o.dataAt ? fmtDateTime(o.dataAt) : '—'}。
            </p>
          </Plate>
        </div>

        <Plate title="订阅" extra={<Link to="/subscriptions" className="small">订阅列表</Link>}>
          <div className="stats" style={{ marginTop: 12 }}>
            <Stat k="全部" v={String(s.total)} to="/subscriptions" />
            <Stat k="可用" v={String(s.usable)} to="/subscriptions?usable=true" mark="ok" />
            <Stat k="待履约" v={String(s.pending)} to="/subscriptions?blocker=pending" tone={s.pending ? 'warn' : undefined} />
            <Stat k="履约异常" v={String(s.fulfillFailed)} to="/subscriptions?blocker=fulfill_failed" tone={s.fulfillFailed ? 'stop' : undefined} />
            <Stat k="变更中" v={String(s.changing)} to="/subscriptions?blocker=changing" />
            <Stat k="数据面异常" v={String(s.dataplane)} to="/subscriptions?blocker=dataplane" tone={s.dataplane ? 'warn' : undefined} />
            <Stat k="流量耗尽" v={String(s.exhausted)} to="/subscriptions?blocker=traffic_exhausted" />
            <Stat k="保留期" v={String(s.expired)} to="/subscriptions?lifecycle=retained" />
            <Stat k="已暂停" v={String(s.paused)} to="/subscriptions?blocker=admin_paused" />
            <Stat k="已释放" v={String(s.released)} to="/subscriptions?lifecycle=released" />
          </div>
        </Plate>

        <Plate title="订单与申请">
          <div className="stats" style={{ marginTop: 12 }}>
            <Stat k="待核款" v={String(o.orders.awaitingPayment)} to="/orders?payment=awaiting" tone={o.orders.awaitingPayment ? 'warn' : undefined} />
            <Stat k="待履约" v={String(o.orders.awaitingFulfillment)} to="/orders?fulfillment=processing" />
            <Stat k="履约失败" v={String(o.orders.fulfillFailed)} to="/orders?fulfillment=failed" tone={o.orders.fulfillFailed ? 'stop' : undefined} />
            <Stat k="退款审核" v={String(o.orders.refundReview)} to="/orders?tab=refunds" tone={o.orders.refundReview ? 'warn' : undefined} />
            <Stat k="换出口待受理" v={String(o.exitRequests.pending)} to="/exit-requests?status=pending" tone={o.exitRequests.pending ? 'warn' : undefined} />
            <Stat k="待定费用" v={String(o.exitRequests.awaitingFeeDecision)} to="/exit-requests?feeStatus=awaiting_decision" />
            <Stat k="换出口处理中" v={String(o.exitRequests.processing)} to="/exit-requests?status=processing" />
          </div>
        </Plate>
      </div>
      {paying && <ConfirmPaymentDialog order={paying} open onClose={() => setPaying(null)} />}
    </>
  )
}

function Stat({ k, v, note, to, tone, mark }: { k: string; v: string | null; note?: string; to?: string; tone?: 'warn' | 'stop'; mark?: 'ok' }) {
  const body = (
    <>
      <span className="stat-k">
        {mark && (
          <span className="state tone-ok">
            <StateMark shape="filled" />
          </span>
        )}
        {k}
      </span>
      <span className="stat-v">{v === null ? <span className="muted" style={{ fontSize: 16, fontWeight: 500 }}>无数据</span> : v}</span>
      {note && <span className="stat-note">{note}</span>}
    </>
  )
  const cls = `stat${tone ? ` tone-${tone}` : ''}`
  return to ? (
    <Link to={to} className={cls}>
      {body}
    </Link>
  ) : (
    <div className={cls}>{body}</div>
  )
}
