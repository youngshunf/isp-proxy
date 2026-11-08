import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, Radio, Result, Skeleton } from 'antd'
import { useState } from 'react'
import { useNavigate } from 'react-router'
import type { OrderInput, Plan } from '@isp/api-client'
import { api, errorText, qk } from '../app/api'
import { useTitle } from '../app/Shell'
import { useIntentKey } from '../lib/intent'
import { OrderSummary } from '../components/OrderSummary'
import { CountryFlag, ExitModeChip } from '@isp/ui'
import { Plate } from '@isp/ui'
import { fmtBytes, fmtMoney } from '../lib/format'

const STOCK = { plenty: '可售', low: '库存紧张', out: '暂时无货' } as const

/** 套餐的城市文字：为空表示不限城市（D-27）。 */
const cityText = (p: Plan) => p.city ?? '不限城市'

export default function BuyPage() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const catalog = useQuery({ queryKey: qk.catalog, queryFn: () => api.getCatalog(), staleTime: 5 * 60_000 })
  const [planId, setPlanId] = useState<string | null>(null)
  const [months, setMonths] = useState<number | null>(null)
  // 不限城市套餐可选城市（D-27）；null = 不限城市（系统分配），下单时不传 city。
  const [city, setCity] = useState<string | null>(null)
  const choosePlan = (id: string) => {
    if (id !== planId) setCity(null)
    setPlanId(id)
  }
  const intent = useIntentKey()
  useTitle('购买 IP')
  const create = useMutation({
    // 一次订单一个 IP（D-13）：quantity 固定为 1；回传价格版本，价格已变化时服务端返回 409。
    mutationFn: (input: OrderInput) => api.createOrder(input, intent.keyFor(input)),
    onSuccess: (o) => {
      intent.reset()
      void qc.invalidateQueries({ queryKey: qk.orders })
      void qc.invalidateQueries({ queryKey: qk.subs })
      navigate(`/orders/${o.id}`)
    },
  })

  if (catalog.isError) {
    return (
      <Plate>
        <Result status="warning" title="套餐没有加载出来" subTitle={errorText(catalog.error)} extra={<Button onClick={() => catalog.refetch()}>重新加载</Button>} />
      </Plate>
    )
  }
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
  const m = months ?? c.monthOptions[0] ?? 1
  const cities = plan && plan.city === null ? plan.cities : []
  // 所选城市已不在可选列表里（例如目录刷新后无货）时按不限城市处理。
  const chosenCity = city !== null && cities.some((x) => x.name === city) ? city : null
  const lines = plan
    ? [
        {
          label: plan.name,
          detail: `${plan.country} · ${chosenCity ?? cityText(plan)} · ${m} 个月 · 含 ${fmtBytes(plan.includedBytes)}`,
          amount: { amount: plan.monthlyPrice.amount * m, currency: 'CNY' as const },
        },
      ]
    : []

  return (
    <>
      <div className="page-head">
        <div>
          <h1>购买 IP</h1>
          <div className="page-head-sub">
            独享套餐的 IP 单独占用一个出口，共享套餐与其他订阅共用一个出口、价格更低；时长按 {c.rules.periodDays} 天一个月，含流量长期保留。一次订单购买 1 个 IP，需要多个请分别下单。
          </div>
        </div>
      </div>
      <div className="buy">
        <Plate title="选择套餐" labelledBy="plans-h">
          <div className="plate-pad">
              <table className="sheet">
                <thead>
                  <tr>
                    <th>套餐 / 地区</th>
                    <th>出口</th>
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
                        onClick={() => !out && choosePlan(p.id)}
                      >
                        <td>
                          <span className="region">
                            <Radio
                              checked={selected}
                              disabled={out}
                              onChange={() => choosePlan(p.id)}
                              aria-label={p.exitMode === 'shared' ? `${p.country} ${cityText(p)} 共享` : `${p.country} ${cityText(p)}`}
                              aria-describedby={`plan-name-${p.id}`}
                            />
                            <CountryFlag code={p.countryCode} name={p.country} size="sm" />
                            <span className="plan-id">
                              <b className="plan-name" id={`plan-name-${p.id}`}>
                                {p.name}
                              </b>
                              <span className="plan-region">
                                {p.country} · {cityText(p)}
                                {p.city === null && p.cities.length > 0 ? ` · 可选 ${p.cities.length} 个城市` : ''}
                              </span>
                            </span>
                          </span>
                        </td>
                        <td className="c-mode">
                          <ExitModeChip mode={p.exitMode} />
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
              具体出口在开通时分配，下单不锁定某个地址；下单即预留库存 {c.rules.unpaidHoldHours} 小时。
            </p>

            {plan && cities.length > 0 && (
              <div className="step" style={{ marginTop: 28 }} role="group" aria-labelledby="city-h">
                <h3 className="step-title" id="city-h">
                  城市
                </h3>
                <div className="choice-row">
                  <button type="button" className="choice" aria-pressed={chosenCity === null} onClick={() => setCity(null)}>
                    <b>不限城市</b>
                    <span>系统分配 · {STOCK[plan.stock]}</span>
                  </button>
                  {cities.map((x) => (
                    <button
                      key={x.name}
                      type="button"
                      className="choice"
                      aria-pressed={chosenCity === x.name}
                      disabled={x.stock === 'out'}
                      onClick={() => setCity(x.name)}
                    >
                      <b>{x.name}</b>
                      <span className={`stock ${x.stock}`}>{STOCK[x.stock]}</span>
                    </button>
                  ))}
                </div>
                <p className="plate-note" style={{ margin: '8px 0 0' }}>
                  不选城市时在{plan.country}全国范围分配出口；选了城市只在该城市分配，该城市无货时不能下单。
                </p>
              </div>
            )}

            <div className="step" style={{ marginTop: 28 }}>
              <h3 className="step-title">时长</h3>
              <div className="choice-row">
                {c.monthOptions.map((n) => (
                  <button key={n} type="button" className="choice" aria-pressed={m === n} onClick={() => setMonths(n)}>
                    <b>{n} 个月</b>
                    <span>{n * c.rules.periodDays} 天{plan ? ` · ${fmtMoney({ amount: plan.monthlyPrice.amount * n, currency: 'CNY' })}` : ''}</span>
                  </button>
                ))}
              </div>
              <p className="plate-note" style={{ margin: '8px 0 0' }}>
                含流量按套餐一次发放，不按月重复发放；不够时随时单独买流量。
              </p>
            </div>

          </div>
        </Plate>
        <OrderSummary
          catalog={c}
          lines={lines}
          rules={[
            plan ? `时长 ${m * c.rules.periodDays} 天 + 流量 ${fmtBytes(plan.includedBytes)}` : '时长与流量随套餐发放',
            ...(plan?.exitMode === 'shared' ? ['共享出口：与其他订阅共用一个出口 IP，接入与流量各自独立'] : []),
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
          onSubmit={() =>
            plan &&
            create.mutate({ kind: 'new', planId: plan.id, planVersionId: plan.versionId, months: m, quantity: 1, ...(chosenCity ? { city: chosenCity } : {}) })
          }
        />
      </div>
    </>
  )
}
