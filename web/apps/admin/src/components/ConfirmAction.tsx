// 破坏性 / 敏感操作的确认框（docs/05 F-W3）：
// - 先写清“将会发生的变化”，用 stop / warn 色调的提示块列出影响；
// - 需要原因的操作必须填写（进入审计）；
// - 提交中禁用按钮，并用 ref 挡住连点，保证一次意图只发一次请求；
// - 失败时留在框内显示服务端给的原因，不自动重试。
import { ExclamationCircleOutlined, WarningOutlined } from '@ant-design/icons'
import { Input, Modal } from 'antd'
import { useEffect, useRef, useState, type ReactNode } from 'react'
import { errorText } from '../app/api'
import { Callout } from './bits'

export interface ConfirmActionProps {
  open: boolean
  title: ReactNode
  /** 将会发生的变化（逐条列出）。 */
  impacts: ReactNode[]
  tone?: 'stop' | 'warn' | 'info'
  okText: string
  /** 需要原因时给出字段名，例如“封禁原因”。 */
  reasonLabel?: string
  reasonPlaceholder?: string
  /** 额外字段（金额、流水号等），放在影响说明之后。 */
  children?: ReactNode
  /** 额外字段是否已填好。 */
  ready?: boolean
  onConfirm: (reason: string) => Promise<unknown>
  onClose: () => void
  onDone?: (result: unknown) => void
}

export function ConfirmAction(p: ConfirmActionProps) {
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const inflight = useRef(false)

  useEffect(() => {
    if (p.open) {
      setReason('')
      setError(null)
    }
  }, [p.open])

  const tone = p.tone ?? 'stop'
  const needReason = Boolean(p.reasonLabel)
  const canSubmit = (!needReason || reason.trim().length > 0) && (p.ready ?? true) && !busy

  const submit = async () => {
    if (inflight.current || !canSubmit) return
    inflight.current = true
    setBusy(true)
    setError(null)
    try {
      const r = await p.onConfirm(reason.trim())
      p.onDone?.(r)
      p.onClose()
    } catch (e) {
      setError(e)
    } finally {
      inflight.current = false
      setBusy(false)
    }
  }

  return (
    <Modal
      open={p.open}
      title={p.title}
      onCancel={() => !busy && p.onClose()}
      onOk={submit}
      okText={p.okText}
      cancelText="取消"
      okButtonProps={{ disabled: !canSubmit, loading: busy, danger: tone === 'stop' }}
      cancelButtonProps={{ disabled: busy }}
      mask={{ closable: !busy }}
      destroyOnHidden
      width={560}
    >
      <div className="confirm-body">
        <Callout tone={tone} icon={tone === 'info' ? <ExclamationCircleOutlined /> : <WarningOutlined />}>
          <div className="impact-title">将会发生的变化</div>
          <ul className="impact-list">
            {p.impacts.map((x, i) => (
              <li key={i}>{x}</li>
            ))}
          </ul>
        </Callout>
        {p.children}
        {needReason && (
          <label className="field">
            <span className="field-label">
              {p.reasonLabel}
              <em>必填，写入审计</em>
            </span>
            <Input.TextArea
              aria-label={p.reasonLabel}
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              placeholder={p.reasonPlaceholder}
              autoSize={{ minRows: 2, maxRows: 5 }}
              maxLength={500}
            />
          </label>
        )}
        {error != null && (
          <Callout tone="stop">
            <b>没有完成：</b>
            {errorText(error)}
          </Callout>
        )}
      </div>
    </Modal>
  )
}
