// 订单详情：明细、进度、收款记录、入账条目、退款；确认收款、取消、重试履约、退款申请与复核。
import { useQuery } from '@tanstack/react-query'
import { Button, Table } from 'antd'
import { useState } from 'react'
import { Link, useParams } from 'react-router'
import type { Refund } from '@isp/api-client/admin'
import { Plate, StateMark, fmtBytes, fmtDateTime, fmtMoney, fmtSpan } from '@isp/ui'
import { qk, useApi } from '../app/api'
import { Callout, Cell, Facts, LoadError, Loading, Mono, PageHead, State, SubLink } from '../components/bits'
import { ConfirmPaymentDialog } from '../components/ConfirmPaymentDialog'
import { CancelOrderDialog, DecideRefundDialog, RequestRefundDialog, RetryOrderDialog } from '../components/orderActions'
import { FULFILLMENT, LEDGER_SOURCE, ORDER_KIND, PAYMENT } from '../lib/status'

type Dlg = 'pay' | 'cancel' | 'retry' | 'refund' | null

export default function OrderDetailPage() {
  const { id = '' } = useParams()
  const api = useApi()
  const q = useQuery({ queryKey: qk.order(id), queryFn: () => api.adminGetOrder({ orderId: id }) })
  const [dlg, setDlg] = useState<Dlg>(null)
  const [deciding, setDeciding] = useState<Refund | null>(null)
  if (q.isPending) return <Loading rows={10} />
  if (q.isError) return <LoadError error={q.error} retry={() => void q.refetch()} />
  const o = q.data
  const pr = o.paymentRecord

  return (
    <>
      <PageHead
        back={{ to: '/orders', label: '订单' }}
        title={
          <>
            <Mono>{o.no}</Mono>
            <span className="cells" style={{ fontSize: 14 }}>
              <Cell k="收款" view={PAYMENT[o.payment]} />
              <Cell k="履约" view={FULFILLMENT[o.fulfillment]} />
            </span>
          </>
        }
        sub={
          <>
            <span>{ORDER_KIND[o.kind]}</span>
            <Link to={`/users/${o.user.id}`}>{o.user.email}</Link>
            {o.subscriptionId && <SubLink id={o.subscriptionId} name={o.subscriptionName} />}
            <span>下单 {fmtDateTime(o.createdAt)}</span>
          </>
        }
        extra={
          <>
            {o.payment === 'awaiting' && (
              <Button type="primary" onClick={() => setDlg('pay')}>
                确认收款
              </Button>
            )}
            {o.payment === 'cancelled' && !pr && <Button onClick={() => setDlg('pay')}>确认晚到款</Button>}
            {o.payment === 'awaiting' && <Button onClick={() => setDlg('cancel')}>取消订单</Button>}
            {o.fulfillment === 'failed' && o.payment === 'confirmed' && <Button onClick={() => setDlg('retry')}>重试履约</Button>}
            {o.payment === 'confirmed' && (
              <Button danger onClick={() => setDlg('refund')}>
                申请退款
              </Button>
            )}
          </>
        }
      />
      <div className="stack">
        {o.fulfillmentError && <Callout tone="stop">履约异常：{o.fulfillmentError}</Callout>}
        {o.payment === 'awaiting' && o.userClaimedPaidAt && (
          <Callout tone="warn">用户于 {fmtDateTime(o.userClaimedPaidAt)} 标记已付款。请按付款备注码 <Mono>{o.payRef}</Mono> 核对流水。</Callout>
        )}
        <div className="grid-5-7">
          <Plate title="订单">
            <div className="plate-pad">
              <Facts
                items={[
                  ['金额', <b key="a" className="amount">{fmtMoney(o.amount)}</b>],
                  ['付款备注码', <Mono key="r">{o.payRef}</Mono>],
                  ['收款方式', o.paymentMethod === 'admin_gift' ? '管理员赠送' : '银行转账'],
                  ['保留至', o.holdUntil ? fmtDateTime(o.holdUntil) : null],
                  ['取消原因', o.cancelReason ? { user: '用户取消', timeout: '超时未付', admin: '管理员取消' }[o.cancelReason] : null],
                  ['确认收款', o.confirmedAt ? fmtDateTime(o.confirmedAt) : null],
                  ['交付', o.deliveredAt ? fmtDateTime(o.deliveredAt) : null],
                  ['规则版本', <Mono key="v">{o.rulesVersion}</Mono>],
                  ['套餐版本', o.planVersionId ? <Mono key="p">{o.planVersionId}</Mono> : null],
                  ['代建人', o.createdByAdmin?.email ?? null],
                ]}
              />
            </div>
          </Plate>
          <div className="stack">
            <Plate title="明细">
              <Table
                className="plain-table"
                size="small"
                rowKey="label"
                pagination={false}
                dataSource={o.lines}
                columns={[
                  { title: '项目', dataIndex: 'label' },
                  { title: '说明', dataIndex: 'detail' },
                  { title: '金额', dataIndex: 'amount', align: 'right', render: (v) => fmtMoney(v) },
                ]}
                style={{ marginTop: 12 }}
              />
            </Plate>
            <Plate title="收款记录">
              <div className="plate-pad">
                {pr ? (
                  <Facts
                    cols={2}
                    items={[
                      ['到账金额', fmtMoney(pr.amount)],
                      ['流水号', <Mono key="r">{pr.externalRef}</Mono>],
                      ['确认人', pr.confirmedBy.email],
                      ['确认时间', fmtDateTime(pr.confirmedAt)],
                      ['晚到款', pr.lateArrival ? '是' : '否'],
                      ['备注', pr.note || null],
                    ]}
                  />
                ) : (
                  <p className="muted">还没有收款记录。</p>
                )}
              </div>
            </Plate>
          </div>
        </div>

        {o.steps.length > 0 && (
          <Plate title="进度">
            <div className="plate-pad">
              <ol className="timeline">
                {o.steps.map((st) => (
                  <li key={st.label}>
                    <span className={`t-mark state tone-${st.state === 'done' ? 'ok' : st.state === 'failed' ? 'stop' : st.state === 'current' ? 'wait' : 'wait'}`}>
                      <StateMark shape={st.state === 'done' ? 'filled' : st.state === 'current' ? 'half' : 'hollow'} />
                    </span>
                    <div>
                      <div className="t-title">{st.label}</div>
                      <div className="t-meta">{st.at ? fmtDateTime(st.at) : st.state === 'todo' ? '未开始' : '—'}</div>
                    </div>
                  </li>
                ))}
              </ol>
            </div>
          </Plate>
        )}

        <Plate title="入账条目">
          <Table
            className="plain-table"
            size="small"
            rowKey="id"
            pagination={false}
            scroll={{ x: 'max-content' }}
            dataSource={o.ledgerEntries}
            locale={{ emptyText: '确认收款后入账' }}
            style={{ marginTop: 12 }}
            columns={[
              { title: '时间', dataIndex: 'createdAt', render: (v: string) => fmtDateTime(v) },
              { title: '来源', dataIndex: 'sourceType', render: (v: keyof typeof LEDGER_SOURCE) => LEDGER_SOURCE[v] },
              { title: '数量', align: 'right', render: (_, e) => (e.unit === 'bytes' ? `${e.amount > 0 ? '+' : ''}${fmtBytes(e.amount)}` : fmtSpan(e.amount, true)) },
              { title: '原因', dataIndex: 'reason' },
            ]}
          />
        </Plate>

        {o.refunds.length > 0 && (
          <Plate title="退款">
            <Table
              className="plain-table"
              size="small"
              rowKey="id"
              pagination={false}
              scroll={{ x: 'max-content' }}
              dataSource={o.refunds}
              style={{ marginTop: 12 }}
              columns={[
                { title: '金额', dataIndex: 'amount', render: (v) => fmtMoney(v) },
                { title: '回收', render: (_, r) => `${fmtBytes(r.reclaimBytes)} · ${fmtSpan(r.reclaimSeconds)}` },
                { title: '原因', dataIndex: 'reason' },
                {
                  title: '状态',
                  dataIndex: 'status',
                  render: (v: Refund['status']) => (
                    <State view={v === 'review' ? { label: '待复核', tone: 'warn', shape: 'half' } : v === 'approved' ? { label: '已批准', tone: 'ok', shape: 'filled' } : { label: '已驳回', tone: 'stop', shape: 'hollow' }} />
                  ),
                },
                { title: '申请', render: (_, r) => `${r.requestedBy.email} · ${fmtDateTime(r.requestedAt)}` },
                {
                  title: '操作',
                  render: (_, r) =>
                    r.status === 'review' ? (
                      <Button size="small" onClick={() => setDeciding(r)}>
                        复核
                      </Button>
                    ) : (
                      (r.decisionNote ?? '—')
                    ),
                },
              ]}
            />
          </Plate>
        )}

        {o.rulesSnapshot.length > 0 && (
          <Plate title="下单时的规则快照">
            <ul className="plate-pad small" style={{ margin: 0, paddingLeft: 40 }}>
              {o.rulesSnapshot.map((l) => (
                <li key={l}>{l}</li>
              ))}
            </ul>
          </Plate>
        )}
      </div>

      <ConfirmPaymentDialog order={o} open={dlg === 'pay'} onClose={() => setDlg(null)} />
      <CancelOrderDialog order={o} open={dlg === 'cancel'} onClose={() => setDlg(null)} />
      <RetryOrderDialog order={o} open={dlg === 'retry'} onClose={() => setDlg(null)} />
      <RequestRefundDialog order={o} open={dlg === 'refund'} onClose={() => setDlg(null)} />
      {deciding && <DecideRefundDialog refund={deciding} orderNo={o.no} open onClose={() => setDeciding(null)} />}
    </>
  )
}
