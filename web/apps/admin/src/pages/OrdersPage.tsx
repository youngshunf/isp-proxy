// 订单（R-12）：筛选、人工核款、取消；“退款审核”标签页处理退款复核。
// 收款与履约是两个独立维度，分两格表达（DESIGN.md Chips）。
import { useQuery } from '@tanstack/react-query'
import { Button, Tabs } from 'antd'
import { useMemo, useState } from 'react'
import { Link, useSearchParams } from 'react-router'
import type { AdminOrder, FulfillmentStatus, OrderKind, PaymentStatus, Refund } from '@isp/api-client/admin'
import { Plate, fmtBytes, fmtDateTime, fmtMoney, fmtSpan } from '@isp/ui'
import { qk, useApi } from '../app/api'
import { Ago, Cell, Mono, PageHead, State, SubLink } from '../components/bits'
import { ConfirmPaymentDialog } from '../components/ConfirmPaymentDialog'
import { DataTable, arr, str, type Column, type FilterDef } from '../components/DataTable'
import { DecideRefundDialog } from '../components/orderActions'
import { FULFILLMENT, ORDER_KIND, PAYMENT, options } from '../lib/status'

const REFUND_STATUS = {
  review: { label: '待复核', tone: 'warn', shape: 'half' },
  approved: { label: '已批准', tone: 'ok', shape: 'filled' },
  rejected: { label: '已驳回', tone: 'stop', shape: 'hollow' },
} as const

export default function OrdersPage() {
  const [sp, setSp] = useSearchParams()
  const tab = sp.get('tab') === 'refunds' ? 'refunds' : 'orders'
  return (
    <>
      <PageHead title="订单" sub="人工核款：核对银行流水的金额和付款备注码后再确认；同一订单只记一次收款。" />
      <Plate className="tabs-plate">
        <Tabs
          activeKey={tab}
          onChange={(k) => setSp(k === 'refunds' ? { tab: 'refunds' } : {}, { replace: true })}
          items={[
            { key: 'orders', label: '订单', children: <OrderTable /> },
            { key: 'refunds', label: '退款审核', children: <RefundTable /> },
          ]}
        />
      </Plate>
    </>
  )
}

function OrderTable() {
  const api = useApi()
  const [paying, setPaying] = useState<AdminOrder | null>(null)
  const filters = useMemo<FilterDef[]>(
    () => [
      { key: 'q', label: '搜索', type: 'search', placeholder: '订单号、付款备注码、用户邮箱', width: 260 },
      { key: 'payment', label: '收款', type: 'multi', options: options(PAYMENT) },
      { key: 'fulfillment', label: '履约', type: 'multi', options: options(FULFILLMENT) },
      { key: 'kind', label: '类型', type: 'select', options: options(ORDER_KIND), width: 120 },
      { key: 'createdFrom', label: '下单起', type: 'date' },
      { key: 'createdTo', label: '下单止', type: 'date' },
    ],
    [],
  )
  const columns = useMemo<Column<AdminOrder>[]>(
    () => [
      {
        key: 'no',
        title: '订单号',
        fixedVisible: true,
        render: (_, o) => (
          <div className="cell-stack">
            <Link to={`/orders/${o.id}`}>
              <Mono>{o.no}</Mono>
            </Link>
            <span className="muted">
              备注码 <Mono>{o.payRef}</Mono>
            </span>
          </div>
        ),
      },
      { key: 'kind', title: '类型', render: (_, o) => ORDER_KIND[o.kind] },
      { key: 'user', title: '用户', render: (_, o) => <Link to={`/users/${o.user.id}`}>{o.user.email}</Link> },
      { key: 'sub', title: '订阅', render: (_, o) => (o.subscriptionId ? <SubLink id={o.subscriptionId} name={o.subscriptionName} /> : <span className="muted">待生成</span>) },
      { key: 'amount', title: '金额', align: 'right', render: (_, o) => <span className="amount">{fmtMoney(o.amount)}</span> },
      {
        key: 'status',
        title: '状态',
        render: (_, o) => (
          <span className="cells">
            <Cell k="收款" view={PAYMENT[o.payment]} />
            <Cell k="履约" view={FULFILLMENT[o.fulfillment]} />
          </span>
        ),
      },
      {
        key: 'claimed',
        title: '用户标记付款',
        render: (_, o) => (o.userClaimedPaidAt ? <Ago at={o.userClaimedPaidAt} /> : <span className="muted">—</span>),
      },
      { key: 'created', title: '下单', render: (_, o) => fmtDateTime(o.createdAt) },
      { key: 'hold', title: '保留至', defaultHidden: true, render: (_, o) => fmtDateTime(o.holdUntil) },
      { key: 'method', title: '收款方式', defaultHidden: true, render: (_, o) => (o.paymentMethod === 'admin_gift' ? '管理员赠送' : '银行转账') },
      { key: 'rules', title: '规则版本', defaultHidden: true, render: (_, o) => <Mono>{o.rulesVersion}</Mono> },
      {
        key: 'op',
        title: '操作',
        fixedVisible: true,
        fixed: 'right',
        render: (_, o) =>
          o.payment === 'awaiting' ? (
            <Button size="small" type="primary" onClick={() => setPaying(o)}>
              确认收款
            </Button>
          ) : (
            <Link to={`/orders/${o.id}`} className="small">
              详情
            </Link>
          ),
      },
    ],
    [],
  )
  return (
    <>
      <DataTable<AdminOrder>
        id="orders"
        label="订单列表"
        columns={columns}
        filters={filters}
        queryKey={qk.orders}
        rowKey={(o) => o.id}
        rowClassName={(o) => (o.payment === 'awaiting' && o.userClaimedPaidAt ? 'row-flag' : '')}
        fetchPage={({ filters: f, cursor, limit }) =>
          api.adminListOrders({
            query: {
              q: str(f, 'q'),
              payment: arr<PaymentStatus>(f, 'payment'),
              fulfillment: arr<FulfillmentStatus>(f, 'fulfillment'),
              kind: str(f, 'kind') as OrderKind | undefined,
              createdFrom: str(f, 'createdFrom'),
              createdTo: str(f, 'createdTo'),
              cursor,
              limit,
            },
          })
        }
      />
      {paying && <ConfirmPaymentDialog order={paying} open onClose={() => setPaying(null)} />}
    </>
  )
}

function RefundTable() {
  const api = useApi()
  const [deciding, setDeciding] = useState<Refund | null>(null)
  const orders = useQuery({ queryKey: [...qk.orders, 'refund-names'], queryFn: () => api.adminListOrders({ query: { payment: ['refund_review', 'refunded'], limit: 200 } }) })
  const orderOf = (id: string) => orders.data?.items.find((o) => o.id === id)
  const filters = useMemo<FilterDef[]>(
    () => [
      {
        key: 'status',
        label: '状态',
        type: 'select',
        options: Object.entries(REFUND_STATUS).map(([value, v]) => ({ value, label: v.label })),
      },
    ],
    [],
  )
  const columns: Column<Refund>[] = [
    { key: 'order', title: '订单', fixedVisible: true, render: (_, r) => <Link to={`/orders/${r.orderId}`}><Mono>{orderOf(r.orderId)?.no ?? r.orderId}</Mono></Link> },
    { key: 'amount', title: '退款金额', align: 'right', render: (_, r) => <span className="amount">{fmtMoney(r.amount)}</span> },
    { key: 'reclaim', title: '回收', render: (_, r) => `流量 ${fmtBytes(r.reclaimBytes)} · 时长 ${fmtSpan(r.reclaimSeconds)}` },
    { key: 'reason', title: '原因', render: (_, r) => r.reason },
    { key: 'by', title: '申请人', render: (_, r) => <div className="cell-stack"><span>{r.requestedBy.email}</span><span className="muted">{fmtDateTime(r.requestedAt)}</span></div> },
    { key: 'status', title: '状态', render: (_, r) => <State view={REFUND_STATUS[r.status]} /> },
    { key: 'decision', title: '复核', render: (_, r) => (r.decidedBy ? <div className="cell-stack"><span>{r.decidedBy.email}</span><span className="muted">{r.decisionNote}</span></div> : '—') },
    {
      key: 'op',
      title: '操作',
      fixedVisible: true,
      fixed: 'right',
      render: (_, r) =>
        r.status === 'review' ? (
          <Button size="small" onClick={() => setDeciding(r)}>
            复核
          </Button>
        ) : r.externalRef ? (
          <Mono>{r.externalRef}</Mono>
        ) : (
          '—'
        ),
    },
  ]
  return (
    <>
      <DataTable<Refund>
        id="refunds"
        label="退款列表"
        columns={columns}
        filters={filters}
        queryKey={qk.refunds}
        rowKey={(r) => r.id}
        fetchPage={({ filters: f, cursor, limit }) => api.adminListRefunds({ query: { status: str(f, 'status') as Refund['status'] | undefined, cursor, limit } })}
      />
      {deciding && <DecideRefundDialog refund={deciding} orderNo={orderOf(deciding.orderId)?.no} open onClose={() => setDeciding(null)} />}
    </>
  )
}
