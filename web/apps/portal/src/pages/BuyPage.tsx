import { MinusOutlined, PlusOutlined } from '@ant-design/icons'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, Radio, Skeleton } from 'antd'
import { useState } from 'react'
import { useNavigate } from 'react-router'
import type { Plan } from '@isp/api-client'
import { api, errorText, qk } from '../app/api'
import { OrderSummary } from '../components/OrderSummary'
import { CountryFlag } from '../components/marks'
import { Plate } from '../components/Plate'
import { fmtBytes, fmtMoney } from '../lib/format'

const STOCK = { plenty: '可售', low: '库存紧张', out: '暂时无货' } as const

export default function BuyPage() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const catalog = useQuery({ queryKey: qk.catalog, queryFn: () => api.getCatalog(), staleTime: 5 * 60_000 })
  const [planId, setPlanId] = useState<string | null>(null)
  const [months, setMonths] = useState(1)
  const [qty, setQty] = useState(1)
  const create = useMutation({
    mutationFn: () => api.createOrder({ kind: 'new', planId: planId!, months, quantity: qty }),
    onSuccess: (o) => {
      void qc.invalidateQueries({ queryKey: qk.orders })
      navigate(`/orders/${o.id}`)
    },
  })

  if (catalog.isPending || !catalog.data) {
    return (
      <Plate>
        <div className="plate-pad">
          <Skeleton active paragraph={{ rows: 8 }} />
        </div>
      </Plate>
    )
  }
  const c = catalog.data
  const plan: Plan | undefined = c.plans.find((p) => p.id === planId)
  const lines = plan
    ? [
        {
          label: `${plan.country} · ${plan.city} 包月 × ${qty}`,
          detail: `${months} 个月 · 每个含 ${fmtBytes(plan.includedBytes)}`,
          amount: { amount: plan.monthlyPrice.amount * months * qty, currency: 'CNY' as const },
        },
      ]
    : []

  return (
    <>
      <div className="page-head">
        <div>
          <h1>购买 IP</h1>
          <div className="page-head-sub">每个 IP 独享一个出口；时长按 30 天一个月，含流量长期保留。</div>
        </div>
      </div>
      <div className="buy">
        <Plate title="选择地区" labelledBy="plans-h">
          <div className="plate-pad">
              <table className="sheet">
                <thead>
                  <tr>
                    <th>地区</th>
                    <th className="num">包月价</th>
                    <th className="num">含流量</th>
                    <th>并发 / 带宽</th>
                    <th>库存</th>
                  </tr>
                </thead>
                <tbody>
                  {c.plans.map((p) => {
                    const out = p.stock === 'out'
                    const selected = p.id === planId
                    return (
                      <tr
                        key={p.id}
                        className={`row${selected ? ' selected' : ''}${out ? ' disabled' : ''}`}
                        onClick={() => !out && setPlanId(p.id)}
                      >
                        <td>
                          <span className="region">
                            <Radio checked={selected} disabled={out} onChange={() => setPlanId(p.id)} aria-label={`${p.country} ${p.city}`} />
                            <CountryFlag code={p.countryCode} name={p.country} size="sm" />
                            <span>
                              {p.country} · {p.city}
                            </span>
                          </span>
                        </td>
                        <td className="num">
                          <span className="price">
                            {fmtMoney(p.monthlyPrice)}
                            <small>/月</small>
                          </span>
                        </td>
                        <td className="num">{fmtBytes(p.includedBytes)}</td>
                        <td className="c-spec">
                          {p.concurrency} 并发 · {p.bandwidthMbps} Mbps
                        </td>
                        <td className="c-stock">
                          <span className={`stock ${p.stock}`}>{STOCK[p.stock]}</span>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            <p className="plate-note" style={{ margin: '12px 0 0' }}>
              具体出口在开通时分配，下单不锁定某个地址。库存为示意，以核款时为准。
            </p>

            <div className="step" style={{ marginTop: 28 }}>
              <h3 className="step-title">时长</h3>
              <div className="choice-row">
                {c.monthOptions.map((m) => (
                  <button key={m} type="button" className="choice" aria-pressed={months === m} onClick={() => setMonths(m)}>
                    <b>{m} 个月</b>
                    <span>{m * c.rules.periodDays} 天{plan ? ` · ${fmtMoney({ amount: plan.monthlyPrice.amount * m, currency: 'CNY' })}` : ''}</span>
                  </button>
                ))}
              </div>
              <p className="plate-note" style={{ margin: '8px 0 0' }}>
                含流量按套餐一次发放，不按月重复发放；不够时随时单独买流量。
              </p>
            </div>

            <div className="step">
              <h3 className="step-title" id="qty-h">
                数量
              </h3>
              <div className="actions" style={{ alignItems: 'center' }} role="group" aria-labelledby="qty-h">
                <Button icon={<MinusOutlined />} aria-label="减少" disabled={qty <= 1} onClick={() => setQty((n) => n - 1)} />
                <span style={{ minWidth: 40, textAlign: 'center', fontSize: 18, fontWeight: 600 }} aria-live="polite">
                  {qty}
                </span>
                <Button icon={<PlusOutlined />} aria-label="增加" disabled={qty >= 5} onClick={() => setQty((n) => n + 1)} />
                <span className="plate-note">每个 IP 是一个独立订阅，各有自己的出口、接入和余量</span>
              </div>
            </div>
          </div>
        </Plate>
        <OrderSummary
          catalog={c}
          lines={lines}
          rules={[
            plan ? `每个 IP：时长 ${months * c.rules.periodDays} 天 + 流量 ${fmtBytes(plan.includedBytes)}` : '时长与流量随套餐发放',
            '时长在开通就绪时起算，不在付款时起算',
            '流量长期保留，不按月清零；到期也不清余额',
            '时长到期或流量用尽，任一发生即停止转发',
            `到期后保留原出口与接入 ${c.rules.retentionDays} 天，期内续时长可恢复`,
            `换接入每 24 小时 ${c.rules.accessChangesPerDay} 次；换出口每 30 天免费 ${c.rules.freeExitChangesPer30Days} 次`,
          ]}
          afterPay="开通"
          submitText={plan ? `提交订单 · ${fmtMoney(lines[0]!.amount)}` : '提交订单'}
          disabled={!plan}
          loading={create.isPending}
          error={create.isError ? errorText(create.error) : null}
          onSubmit={() => create.mutate()}
        />
      </div>
    </>
  )
}
