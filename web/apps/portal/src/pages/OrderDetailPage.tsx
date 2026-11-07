import { ArrowLeftOutlined, CheckOutlined, CopyOutlined } from '@ant-design/icons'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, App, Button, Popconfirm, Result, Skeleton } from 'antd'
import { Link, useNavigate, useParams } from 'react-router'
import { orderInFlight } from '@isp/api-client'
import { POLL_MS, api, errorText, qk } from '../app/api'
import { useTitle } from '../app/Shell'
import { StateMark } from '@isp/ui'
import { Plate } from '@isp/ui'
import { useCopy } from '../components/useCopy'
import { fmtDate, fmtMoney } from '../lib/format'
import { KIND_LABEL, OrderChips } from './OrdersPage'

export default function OrderDetailPage() {
  const { id = '' } = useParams()
  const navigate = useNavigate()
  const qc = useQueryClient()
  const { message } = App.useApp()
  const { copied, announce, copy } = useCopy()
  // 待核款、开通中/入账中按 POLL_MS 轮询，进入终态（已交付、已取消、已退款、开通失败）停止。
  const q = useQuery({
    queryKey: qk.order(id),
    queryFn: () => api.getOrder(id),
    refetchInterval: (query) => (query.state.data && orderInFlight(query.state.data) ? POLL_MS : false),
  })
  useTitle(q.data ? `订单 ${q.data.no}` : '订单')
  const catalog = useQuery({ queryKey: qk.catalog, queryFn: () => api.getCatalog(), staleTime: 5 * 60_000 })
  const done = (text: string) => () => {
    void qc.invalidateQueries({ queryKey: qk.orders })
    void qc.invalidateQueries({ queryKey: qk.subs })
    message.success(text)
  }
  const claim = useMutation({ mutationFn: () => api.claimPaid(id), onSuccess: done('已通知管理员核款') })
  const cancel = useMutation({ mutationFn: () => api.cancelOrder(id), onSuccess: done('订单已取消') })

  if (q.isPending) {
    return (
      <Plate>
        <div className="plate-pad">
          <Skeleton active paragraph={{ rows: 8 }} />
        </div>
      </Plate>
    )
  }
  if (q.isError) {
    return <Result status="warning" title="没有找到这个订单" subTitle={errorText(q.error)} extra={<Button onClick={() => navigate('/orders')}>返回订单</Button>} />
  }
  const o = q.data
  const pay = catalog.data?.payment
  const awaiting = o.payment === 'awaiting'

  return (
    <>
      <Link className="back-link" to="/orders">
        <ArrowLeftOutlined /> 订单
      </Link>
      <div className="page-head">
        <div>
          <h1>
            {KIND_LABEL[o.kind]}
            {o.subscriptionName ? ` · ${o.subscriptionName}` : ''}
          </h1>
          <div className="page-head-sub">
            <span>
              订单号 <span className="mono">{o.no}</span>
            </span>
            <span>下单于 {fmtDate(o.createdAt)}</span>
          </div>
        </div>
        <OrderChips o={o} />
      </div>

      <div className="detail">
        <div className="detail-col">
          <Plate title="进度" labelledBy="steps-h">
            <div className="plate-pad">
              <ol className="timeline">
                {o.steps.map((s) => {
                  const color = s.state === 'failed' ? 'var(--stop)' : s.state === 'todo' ? 'var(--ink-3)' : 'var(--teal)'
                  const shape = s.state === 'done' ? 'filled' : s.state === 'current' ? 'half' : 'hollow'
                  return (
                    <li key={s.label}>
                      <span className="t-mark" style={{ color }}>
                        <StateMark shape={shape} size={14} />
                      </span>
                      <div>
                        <div className="t-title" style={{ color: s.state === 'todo' ? 'var(--ink-3)' : undefined }}>
                          {s.label}
                          {s.state === 'current' && <span className="plate-note">（进行中）</span>}
                        </div>
                        {s.at && <div className="t-meta">{fmtDate(s.at)}</div>}
                      </div>
                    </li>
                  )
                })}
              </ol>
              {o.subscriptionId && o.fulfillment === 'delivered' && (
                <p style={{ margin: '16px 0 0' }}>
                  <Link to={`/ips/${o.subscriptionId}`}>查看这个 IP</Link>
                </p>
              )}
            </div>
          </Plate>

          <Plate title="内容与规则快照" labelledBy="lines-h">
            <div className="plate-pad">
              <div className="summary-lines">
                {o.lines.map((l) => (
                  <div className="summary-line" key={l.label}>
                    <div>
                      <div>{l.label}</div>
                      <div className="d">{l.detail}</div>
                    </div>
                    <div>{fmtMoney(l.amount)}</div>
                  </div>
                ))}
              </div>
              <div className="summary-total">
                <span>合计（CNY）</span>
                <b>{fmtMoney(o.amount)}</b>
              </div>
              <ul className="rules">
                {o.rulesSnapshot.map((r) => (
                  <li key={r}>
                    <CheckOutlined />
                    <span>{r}</span>
                  </li>
                ))}
              </ul>
            </div>
          </Plate>
        </div>

        <div className="detail-col">
          {awaiting ? (
            <Plate title="付款方式" labelledBy="pay-h">
              <div className="plate-pad">
                <p style={{ marginTop: 0 }}>
                  请转账 <b style={{ fontSize: 18 }}>{fmtMoney(o.amount)}</b>，并在附言里写上付款识别码：
                </p>
                <div style={{ display: 'flex', alignItems: 'center', gap: 14, margin: '6px 0 18px' }}>
                  <span className="pay-code">{o.payRef}</span>
                  <Button icon={copied === 'ref' ? <CheckOutlined /> : <CopyOutlined />} onClick={() => copy('ref', o.payRef, '付款识别码')}>
                    {copied === 'ref' ? '已复制' : '复制识别码'}
                  </Button>
                  <span className="visually-hidden" aria-live="polite">
                    {announce}
                  </span>
                </div>
                {pay && (
                  <dl className="kv" style={{ gridTemplateColumns: '96px minmax(0,1fr) auto' }}>
                    <dt>收款方</dt>
                    <dd className="value">{pay.payee}</dd>
                    <dd className="act" />
                    <dt>开户行</dt>
                    <dd className="value">{pay.bank}</dd>
                    <dd className="act" />
                    <dt>账号</dt>
                    <dd className="value mono">{pay.account}</dd>
                    <dd className="act" />
                  </dl>
                )}
                <p className="plate-note">{pay?.note}</p>
                {o.holdUntil && (
                  <p className="plate-note">
                    {fmtDate(o.holdUntil)} 前未确认收款，订单自动取消{o.kind === 'new' ? '并释放预留库存' : ''}。
                  </p>
                )}
                {o.userClaimedPaidAt ? (
                  <Alert
                    type="info"
                    showIcon
                    title={`你在 ${fmtDate(o.userClaimedPaidAt)} 反馈了已付款`}
                    description="反馈只是给管理员的核对线索，收款状态以管理员确认为准。"
                  />
                ) : (
                  <div className="actions">
                    <Button type="primary" loading={claim.isPending} onClick={() => claim.mutate()}>
                      我已转账，通知核款
                    </Button>
                    <Popconfirm title="取消这个订单？" okText="取消订单" cancelText="不取消" onConfirm={() => cancel.mutate()}>
                      <Button loading={cancel.isPending}>取消订单</Button>
                    </Popconfirm>
                  </div>
                )}
                {(claim.isError || cancel.isError) && (
                  <Alert style={{ marginTop: 12 }} type="error" showIcon title={errorText(claim.error ?? cancel.error)} />
                )}
              </div>
            </Plate>
          ) : (
            <Plate title="收款" labelledBy="pay-h">
              <div className="plate-pad">
                <dl className="kv">
                  <dt>状态</dt>
                  <dd className="value">
                    <OrderChips o={o} />
                  </dd>
                  <dd className="act" />
                  <dt>识别码</dt>
                  <dd className="value mono">{o.payRef}</dd>
                  <dd className="act" />
                </dl>
                {o.payment === 'cancelled' && <p className="plate-note">已取消的订单如有晚到款，管理员会人工处理：重新开通或退款。</p>}
              </div>
            </Plate>
          )}
        </div>
      </div>
    </>
  )
}
