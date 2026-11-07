import { CheckOutlined } from '@ant-design/icons'
import { Alert, Button } from 'antd'
import type { Catalog, Money } from '@isp/api-client'
import { fmtMoney } from '../lib/format'
import { Plate } from './Plate'

export interface SummaryLine {
  label: string
  detail: string
  amount: Money
}

/** 右侧订单摘要：金额、下单时保存的规则快照、人工核款说明。 */
export function OrderSummary({
  catalog,
  lines,
  rules,
  submitText,
  afterPay,
  disabled,
  loading,
  error,
  onSubmit,
}: {
  catalog: Catalog
  lines: SummaryLine[]
  rules: string[]
  submitText: string
  afterPay: '开通' | '入账'
  disabled?: boolean
  loading?: boolean
  error?: string | null
  onSubmit: () => void
}) {
  const total = lines.reduce((n, l) => n + l.amount.amount, 0)
  return (
    <Plate title="订单摘要" labelledBy="summary-h" className="sticky">
      <div className="plate-pad">
        <div className="summary-lines">
          {lines.map((l) => (
            <div className="summary-line" key={l.label}>
              <div>
                <div>{l.label}</div>
                <div className="d">{l.detail}</div>
              </div>
              <div style={{ whiteSpace: 'nowrap' }}>{fmtMoney(l.amount)}</div>
            </div>
          ))}
          {lines.length === 0 && <p className="plate-note">先选择左侧的选项</p>}
        </div>
        <div className="summary-total">
          <span>应付（CNY）</span>
          <b>{fmtMoney({ amount: total, currency: 'CNY' })}</b>
        </div>
        <ul className="rules" aria-label="下单时保存的规则">
          {rules.map((r) => (
            <li key={r}>
              <CheckOutlined />
              <span>{r}</span>
            </li>
          ))}
        </ul>
        <div className="callout info" style={{ marginTop: 16, fontSize: 13 }}>
          <div>
            <b>人工核款：</b>下单后按订单页的收款信息转账，管理员核对到账后{afterPay}。未付款订单保留 {catalog.rules.unpaidHoldHours} 小时后自动取消。
          </div>
        </div>
        {error && <Alert style={{ marginTop: 12 }} type="error" showIcon title={error} />}
        <Button
          type="primary"
          size="large"
          block
          style={{ marginTop: 16 }}
          disabled={disabled || lines.length === 0}
          loading={loading}
          onClick={onSubmit}
        >
          {submitText}
        </Button>
        <p className="plate-note" style={{ margin: '10px 0 0' }}>
          规则版本 {catalog.rules.version}，下单时一并保存；之后改价或下架不影响这笔订单。
        </p>
      </div>
    </Plate>
  )
}
