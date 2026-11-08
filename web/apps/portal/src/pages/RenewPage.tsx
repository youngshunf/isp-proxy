import { ArrowLeftOutlined, ArrowRightOutlined, WarningOutlined } from '@ant-design/icons'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Result, Segmented, Skeleton, Button } from 'antd'
import { useState } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router'
import { isIsp, remainingBytes, type OrderInput } from '@isp/api-client'
import { api, errorText, qk } from '../app/api'
import { useTitle } from '../app/Shell'
import { useIntentKey } from '../lib/intent'
import { OrderSummary, type SummaryLine } from '../components/OrderSummary'
import { Plate } from '@isp/ui'
import { ExitFace, StateCell } from '../components/SimCard'
import { CountryFlag } from '@isp/ui'
import { addDays, fmtBytes, fmtDate, fmtMoney } from '../lib/format'

type Kind = 'time' | 'traffic'

export default function RenewPage() {
  const { id = '' } = useParams()
  const [params, setParams] = useSearchParams()
  const kind: Kind = params.get('kind') === 'traffic' ? 'traffic' : 'time'
  const navigate = useNavigate()
  const qc = useQueryClient()
  const sub = useQuery({ queryKey: qk.sub(id), queryFn: () => api.getSubscription(id) })
  const catalog = useQuery({ queryKey: qk.catalog, queryFn: () => api.getCatalog(), staleTime: 5 * 60_000 })
  const [months, setMonths] = useState(1)
  const [packId, setPackId] = useState<string | null>(null)
  const intent = useIntentKey()
  useTitle(kind === 'time' ? '续时长' : '买流量')
  const create = useMutation({
    mutationFn: (input: OrderInput) => api.createOrder(input, intent.keyFor(input)),
    onSuccess: (o) => {
      intent.reset()
      void qc.invalidateQueries({ queryKey: qk.orders })
      void qc.invalidateQueries({ queryKey: qk.subs })
      navigate(`/orders/${o.id}`)
    },
  })

  if (sub.isError) {
    return <Result status="warning" title="没有找到这个 IP" extra={<Button onClick={() => navigate('/ips')}>返回我的 IP</Button>} />
  }
  if (!sub.data || !catalog.data) {
    return (
      <Plate>
        <div className="plate-pad">
          <Skeleton active paragraph={{ rows: 6 }} />
        </div>
      </Plate>
    )
  }
  const s = sub.data
  if (!isIsp(s)) {
    // 服务器直出代理（D-25）跟随付费 IP 有效且不限流量，没有续购入口。
    return (
      <Result
        status="info"
        title="服务器直出代理不需要续购"
        subTitle="它跟随你的付费 IP 有效、不限流量；续你的付费 IP 即可延长。"
        extra={<Button onClick={() => navigate(`/ips/${id}`)}>返回详情</Button>}
      />
    )
  }
  const c = catalog.data
  // 续时长按该地区当前上架套餐的价格估算；最终金额以服务端生成的订单为准。
  const plan = c.plans.find((p) => p.countryCode === s.plan.countryCode && p.city === s.plan.city)
  const monthly = plan?.monthlyPrice.amount ?? null
  const retained = s.lifecycle === 'retained'
  const expired = s.blockers.includes('expired') || s.blockers.includes('released')
  const pack = c.trafficPacks.find((p) => p.id === packId)

  const lines: SummaryLine[] =
    kind === 'time'
      ? monthly === null
        ? []
        : [{ label: '续时长', detail: `+${months * c.rules.periodDays} 天 · 不改流量`, amount: { amount: monthly * months, currency: 'CNY' } }]
      : pack
        ? [{ label: '买流量', detail: `+${fmtBytes(pack.bytes)} · 不改到期时间`, amount: pack.price }]
        : []

  const timePreview =
    retained || expired
      ? '从重新就绪时起算'
      : s.time.expiresAt
        ? `${fmtDate(s.time.expiresAt)} → ${fmtDate(addDays(s.time.expiresAt, months * c.rules.periodDays))}`
        : '—'

  return (
    <>
      <Link className="back-link" to={`/ips/${s.id}`}>
        <ArrowLeftOutlined /> {s.name}
      </Link>
      <div className="page-head">
        <div>
          <h1>{kind === 'time' ? '续时长' : '买流量'}</h1>
          <div className="page-head-sub">两项分别购买：续时长不改流量，买流量不改到期时间。</div>
          <Segmented
            style={{ marginTop: 14 }}
          size="large"
          value={kind}
          onChange={(v) => setParams({ kind: String(v) }, { replace: true })}
          options={[
            { label: '续时长', value: 'time' },
            { label: '买流量', value: 'traffic' },
          ]}
          />
        </div>
      </div>
      <div className="buy">
        <div className="detail-col">
          <div className="sim-wrap">
            <section className="plate sim" aria-label={`${s.name} 当前状态`}>
              <div className="plate-body">
                <StateCell sub={s} />
                <div className="sim-face">
                  <div className="sim-top">
                    <CountryFlag code={s.exit?.countryCode ?? s.plan.countryCode} name={s.exit?.country ?? s.plan.country} />
                    <div className="sim-title">
                      <div className="sim-name">{s.name}</div>
                      <div className="sim-plan">{s.plan.name}</div>
                    </div>
                  </div>
                  <ExitFace sub={s} />
                </div>
              </div>
            </section>
          </div>

          <Plate title={kind === 'time' ? '选择时长' : '选择流量包'} labelledBy="opt-h">
            <div className="plate-pad">
              {kind === 'time' ? (
                <>
                  <div className="choice-row">
                    {c.monthOptions.map((m) => (
                      <button key={m} type="button" className="choice" aria-pressed={months === m} onClick={() => setMonths(m)}>
                        <b>+{m * c.rules.periodDays} 天</b>
                        <span>{monthly === null ? '—' : fmtMoney({ amount: monthly * m, currency: 'CNY' })}</span>
                      </button>
                    ))}
                  </div>
                  <div className="preview-change">
                    到期时间 <b>{timePreview}</b>
                  </div>
                  {monthly === null && (
                    <div className="callout warn" style={{ marginTop: 14 }}>
                      <WarningOutlined />
                      <div>该地区的套餐已下架，暂时不能自助续时长，请联系客服。</div>
                    </div>
                  )}
                  {retained && (
                    <div className="callout info" style={{ marginTop: 14 }}>
                      <div>
                        保留期内续时长：恢复原出口 <span className="mono">{s.exit?.ip}</span> 和原接入，程序不用改配置；时长从重新就绪时起算。
                      </div>
                    </div>
                  )}
                  {s.blockers.includes('traffic_exhausted') && (
                    <div className="callout warn" style={{ marginTop: 14 }}>
                      <WarningOutlined />
                      <div>流量已用尽：只续时长不能恢复使用，还需要买流量。</div>
                    </div>
                  )}
                </>
              ) : (
                <>
                  <div className="choice-row">
                    {c.trafficPacks.map((p) => (
                      <button key={p.id} type="button" className="choice" aria-pressed={packId === p.id} onClick={() => setPackId(p.id)}>
                        <b>+{fmtBytes(p.bytes)}</b>
                        <span>{fmtMoney(p.price)}</span>
                      </button>
                    ))}
                  </div>
                  <div className="preview-change">
                    剩余流量 <b>{fmtBytes(remainingBytes(s))}</b>
                    <ArrowRightOutlined style={{ fontSize: 11 }} />
                    <b>{pack ? fmtBytes(remainingBytes(s) + pack.bytes) : '—'}</b>
                  </div>
                  {expired && (
                    <div className="callout warn" style={{ marginTop: 14 }}>
                      <WarningOutlined />
                      <div>这个 IP 已到期：买流量后余额会保留，但还需要续时长才能使用。</div>
                    </div>
                  )}
                </>
              )}
            </div>
          </Plate>
        </div>
        <OrderSummary
          catalog={c}
          lines={lines}
          rules={
            kind === 'time'
              ? [
                  '续时长只延长时长，不改流量',
                  retained || expired ? '到期后续费：从重新就绪时起算' : '有效期内续费：接在原到期时间之后',
                  '核款确认后入账，同一订单只入账一次',
                ]
              : ['买流量只增加流量，不改到期时间', '流量长期保留，不按月清零', '核款确认后入账，同一订单只入账一次']
          }
          afterPay="入账"
          submitText={lines[0] ? `提交订单 · ${fmtMoney(lines[0].amount)}` : '提交订单'}
          loading={create.isPending}
          error={create.isError ? errorText(create.error) : null}
          onSubmit={() => {
            if (kind === 'time') create.mutate({ kind: 'renew_time', subscriptionId: id, months })
            else if (pack) create.mutate({ kind: 'add_traffic', subscriptionId: id, packId: pack.id, packVersionId: pack.versionId })
          }}
        />
      </div>
    </>
  )
}
