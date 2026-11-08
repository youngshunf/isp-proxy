// 用户详情里的“服务器直出代理”（D-25）：勾选赠送 + 带宽上限；取消勾选即收回（确认框列出影响，原因必填）。
// 直出代理经节点自身地址出站、不经 ISP 上游，只限带宽不限流量，跟随用户付费订阅的最晚到期时间有效。
import { useQueryClient } from '@tanstack/react-query'
import { App, Button, Checkbox, InputNumber } from 'antd'
import { useEffect, useState } from 'react'
import type { AdminUserDetail } from '@isp/api-client/admin'
import { Plate } from '@isp/ui'
import { errorText, useApi } from '../app/api'
import { ConfirmAction } from './ConfirmAction'
import { SubLink } from './bits'
import { LIFECYCLE } from '../lib/status'

const MIN_MBPS = 1
const MAX_MBPS = 10_000

export function DirectGrant({ user }: { user: AdminUserDetail }) {
  const api = useApi()
  const qc = useQueryClient()
  const { message } = App.useApp()
  const d = user.direct
  const granted = d.subscriptionId !== null
  const [checked, setChecked] = useState(granted)
  const [mbps, setMbps] = useState<number | null>(d.bandwidthMbps ?? d.defaultBandwidthMbps)
  const [busy, setBusy] = useState(false)
  const [revoking, setRevoking] = useState(false)

  useEffect(() => {
    setChecked(granted)
    setMbps(d.bandwidthMbps ?? d.defaultBandwidthMbps)
  }, [granted, d.bandwidthMbps, d.defaultBandwidthMbps])

  const valid = mbps !== null && Number.isInteger(mbps) && mbps >= MIN_MBPS && mbps <= MAX_MBPS
  const failed = d.lifecycle === 'fulfill_failed'
  const changed = !granted || failed || mbps !== d.bandwidthMbps
  const banned = user.status === 'banned'

  const onToggle = (next: boolean) => {
    if (!next && granted) {
      setRevoking(true)
      return
    }
    setChecked(next)
  }

  const save = async () => {
    if (!valid || busy) return
    setBusy(true)
    try {
      await api.adminGrantDirect({ userId: user.id, body: { bandwidthMbps: mbps } })
      message.success(granted ? (failed ? '已重新下发' : '已更新带宽上限') : '已赠送服务器直出代理')
      void qc.invalidateQueries({ queryKey: ['admin'] })
    } catch (e) {
      message.error(errorText(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Plate title="服务器直出代理" extra={<span className="muted small">管理员赠送 · 不限流量 · 跟随付费订阅有效</span>}>
      <div className="plate-pad stack" style={{ gap: 12 }}>
        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 16 }}>
          <Checkbox checked={checked} onChange={(e) => onToggle(e.target.checked)} disabled={busy || (banned && !granted)}>
            赠送服务器直出代理
          </Checkbox>
          {checked && (
            <>
              <label style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
                <span>带宽上限</span>
                <InputNumber
                  aria-label="带宽上限（Mbps）"
                  min={MIN_MBPS}
                  max={MAX_MBPS}
                  precision={0}
                  value={mbps}
                  onChange={(v) => setMbps(v)}
                  addonAfter="Mbps"
                  style={{ width: 160 }}
                />
              </label>
              <Button type="primary" onClick={() => void save()} loading={busy} disabled={!valid || !changed}>
                {!granted ? '赠送' : failed ? '重新下发' : '保存带宽'}
              </Button>
            </>
          )}
        </div>
        <p className="muted small" style={{ margin: 0 }}>
          {granted ? (
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
              {d.lifecycle && <span>{LIFECYCLE[d.lifecycle]}</span>}
              <SubLink id={d.subscriptionId!} name="查看直出订阅" />
              <span>
                并发 {d.maxConnections} · 带宽 {d.bandwidthMbps} Mbps · 经节点自身地址出站，取消勾选即收回
              </span>
            </span>
          ) : banned ? (
            '账号已封禁，不能赠送。'
          ) : (
            `经节点自身地址出站、不经 ISP 上游；并发 ${d.maxConnections}，带宽默认 ${d.defaultBandwidthMbps} Mbps；没有时长有效的付费订阅时停止转发。`
          )}
        </p>
      </div>
      <ConfirmAction
        open={revoking}
        onClose={() => setRevoking(false)}
        title={`收回 ${user.email} 的服务器直出代理`}
        okText="收回"
        reasonLabel="收回原因"
        impacts={[
          <>直出订阅立即删除，现有连接 1 秒内断开。</>,
          <>接入凭据作废，接入端口进入冷却期，不会立刻分给别人。</>,
          <>付费订阅、账本与订单不受影响；之后可以重新赠送（会分配新的接入）。</>,
        ]}
        onConfirm={(reason) => api.adminRevokeDirect({ userId: user.id, query: { reason } })}
        onDone={() => {
          message.success('已收回服务器直出代理')
          setChecked(false)
          void qc.invalidateQueries({ queryKey: ['admin'] })
        }}
      />
    </Plate>
  )
}
