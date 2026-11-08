// 登记或修改节点（docs/06 §5 adminCreateNode / adminUpdateNode）。
// 登记时填名称、公共地址与端口池；端口池登记后不能改（已分配的端口在用）。修改只改名称、公共地址、状态与直出出口地域。
// 直出出口地域（D-25）：这台服务器自身出站所在的国家/地区，前台直出代理只显示它、不显示出口 IP；只用于展示。
// 节点证书不在这里签发：登记后按部署文档用 isp-control dev-certs 或部署流程签发，再启动 agent。
import { useQueryClient } from '@tanstack/react-query'
import { App, Input, InputNumber, Modal, Segmented } from 'antd'
import { useEffect, useState } from 'react'
import type { Node } from '@isp/api-client/admin'
import { errorText, qk, useApi } from '../app/api'
import { Callout, Mono } from './bits'

type Status = Node['status']

const STATUS_NOTE: Record<Status, string> = {
  active: '正常接受新订阅与换出口。',
  draining: '不再分配新订阅与新候选，已有订阅照常转发。用于计划下线前腾挪。',
  disabled: '停用：不再分配任何新资源。已有订阅需要先迁走，否则会随节点一起不可用。',
}

export function NodeDialog({ value, onClose, onSaved }: { value: Node | 'new' | null; onClose: () => void; onSaved?: (n: Node) => void }) {
  const api = useApi()
  const qc = useQueryClient()
  const { message } = App.useApp()
  const editing = value && value !== 'new' ? value : null
  const [name, setName] = useState('')
  const [host, setHost] = useState('')
  const [start, setStart] = useState<number | null>(20000)
  const [end, setEnd] = useState<number | null>(29999)
  const [status, setStatus] = useState<Status>('active')
  const [regionCode, setRegionCode] = useState('')
  const [regionName, setRegionName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!value) return
    setError(null)
    setName(editing?.name ?? '')
    setHost(editing?.publicHost ?? '')
    setStart(editing?.portRange.start ?? 20000)
    setEnd(editing?.portRange.end ?? 29999)
    setStatus(editing?.status ?? 'active')
    setRegionCode(editing?.directRegion?.countryCode ?? '')
    setRegionName(editing?.directRegion?.name ?? '')
  }, [value]) // 只在打开时按当前值重置表单

  const rangeOk = editing || (start !== null && end !== null && start >= 1024 && end <= 65535 && start <= end)
  // 出口地域：两项都空 = 不填（清除）；否则代码须为两位字母、名称 1～32 字。
  const code = regionCode.trim().toLowerCase()
  const rname = regionName.trim()
  const regionEmpty = code === '' && rname === ''
  const regionOk = regionEmpty || (/^[a-z]{2}$/.test(code) && rname.length >= 1 && rname.length <= 32)
  const region = regionEmpty ? null : { countryCode: code, name: rname }
  const regionChanged =
    (editing?.directRegion?.countryCode ?? '') !== (region?.countryCode ?? '') || (editing?.directRegion?.name ?? '') !== (region?.name ?? '')
  const ready = name.trim().length > 0 && host.trim().length > 0 && !!rangeOk && regionOk

  const submit = async () => {
    if (!ready || busy) return
    setBusy(true)
    setError(null)
    try {
      const n = editing
        ? await api.adminUpdateNode({
            nodeId: editing.id,
            body: {
              name: name.trim() !== editing.name ? name.trim() : undefined,
              publicHost: host.trim() !== editing.publicHost ? host.trim() : undefined,
              status: status !== editing.status ? status : undefined,
              directRegion: regionChanged ? region : undefined,
            },
          })
        : await api.adminCreateNode({
            body: { name: name.trim(), publicHost: host.trim(), portRange: { start: start!, end: end! }, directRegion: region ?? undefined },
          })
      message.success(editing ? '已保存' : '节点已登记')
      void qc.invalidateQueries({ queryKey: qk.nodes })
      onSaved?.(n)
      onClose()
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open={!!value}
      title={editing ? `修改节点 ${editing.name}` : '登记节点'}
      onCancel={() => !busy && onClose()}
      onOk={submit}
      okText={editing ? '保存' : '登记'}
      cancelText="取消"
      okButtonProps={{ disabled: !ready, loading: busy }}
      destroyOnHidden
      width={560}
    >
      <div className="confirm-body">
        {editing ? (
          <p className="muted small">
            <Mono>{editing.id}</Mono> · 端口池 {editing.portRange.start}–{editing.portRange.end}（登记后不能修改）
          </p>
        ) : (
          <Callout tone="info">登记后用节点 ID 签发证书（isp-control dev-certs 或部署流程），再在节点上启动 agent；首次连上控制面后显示“在线”。</Callout>
        )}
        <div className="form-grid">
          <label className="field">
            <span className="field-label">名称</span>
            <Input aria-label="节点名称" value={name} onChange={(e) => setName(e.target.value)} maxLength={64} placeholder="例如 lax-02" />
          </label>
          <label className="field">
            <span className="field-label">
              公共地址<em>客户连接串里的主机</em>
            </span>
            <Input aria-label="公共地址" className="mono" value={host} onChange={(e) => setHost(e.target.value)} maxLength={253} placeholder="lax-02.example.net" />
          </label>
          {!editing && (
            <>
              <label className="field" style={{ marginTop: 12 }}>
                <span className="field-label">端口池起</span>
                <InputNumber aria-label="端口池起" min={1024} max={65535} value={start} onChange={setStart} style={{ width: '100%' }} />
              </label>
              <label className="field" style={{ marginTop: 12 }}>
                <span className="field-label">端口池止</span>
                <InputNumber aria-label="端口池止" min={1024} max={65535} value={end} onChange={setEnd} style={{ width: '100%' }} />
              </label>
            </>
          )}
        </div>
        {!editing && !rangeOk && <Callout tone="warn">端口池须在 1024–65535 之间，且起始不大于结束。</Callout>}
        <div className="form-grid">
          <label className="field">
            <span className="field-label">
              直出出口地域<em>前台直出代理显示的地域名</em>
            </span>
            <Input aria-label="直出出口地域" value={regionName} onChange={(e) => setRegionName(e.target.value)} maxLength={32} placeholder="例如 新加坡、香港" />
          </label>
          <label className="field">
            <span className="field-label">
              地域代码<em>两位字母，决定国旗</em>
            </span>
            <Input aria-label="地域代码" className="mono" value={regionCode} onChange={(e) => setRegionCode(e.target.value)} maxLength={2} placeholder="sg、hk、us" />
          </label>
        </div>
        <p className="muted small">直出代理经这台服务器自身地址出站，前台只显示这里填的地域，不显示 IP。两项都留空即不显示地域。</p>
        {!regionOk && <Callout tone="warn">地域代码须为两位字母（如 sg、hk），地域名 1～32 个字；两项要么都填，要么都空。</Callout>}
        {editing && (
          <div className="field">
            <span className="field-label">状态</span>
            <Segmented
              value={status}
              onChange={(v) => setStatus(v as Status)}
              options={[
                { value: 'active', label: '正常' },
                { value: 'draining', label: '排空' },
                { value: 'disabled', label: '停用' },
              ]}
            />
            <p className="muted small" style={{ marginTop: 6 }}>
              {STATUS_NOTE[status]}
            </p>
          </div>
        )}
        {error && <Callout tone="stop">{error}</Callout>}
      </div>
    </Modal>
  )
}
