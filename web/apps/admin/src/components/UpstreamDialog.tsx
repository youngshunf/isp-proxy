// 录入或编辑一条上游。上游库存页与换出口向导（当场录入候选）共用。
import { useQueryClient } from '@tanstack/react-query'
import { App, Input, InputNumber, Modal, Segmented, Select } from 'antd'
import { useEffect, useState } from 'react'
import type { Upstream } from '@isp/api-client/admin'
import { errorText, qk, useApi } from '../app/api'
import { Callout } from './bits'

export const COUNTRIES = [
  { value: 'us', label: '美国' },
  { value: 'gb', label: '英国' },
  { value: 'de', label: '德国' },
  { value: 'jp', label: '日本' },
  { value: 'sg', label: '新加坡' },
  { value: 'ca', label: '加拿大' },
]

export interface FormState {
  mode: 'url' | 'fields'
  url: string
  protocol: 'http' | 'https' | 'socks5'
  host: string
  port: number | null
  username: string
  password: string
  nodeId?: string
  countryCode?: string
  /** 城市可选（D-27）：留空表示供应商没有给出城市。 */
  city: string
  provider: string
  note: string
  /** 绑定上限（D-24）；null = 新录入时用服务端默认值（core.shared_exit_default_capacity）。 */
  maxTenants: number | null
}

export function UpstreamDialog({
  value,
  nodes,
  onClose,
  defaults,
  onCreated,
}: {
  value: Upstream | 'new' | null
  nodes: { value: string; label: string }[]
  onClose: () => void
  /** 新录入时的预填（换出口向导里按订阅的节点与套餐地区预填）。 */
  defaults?: Partial<FormState>
  /** 新录入成功后回调（换出口向导用它直接选中新候选）。 */
  onCreated?: (u: Upstream) => void
}) {
  const api = useApi()
  const qc = useQueryClient()
  const { message } = App.useApp()
  const editing = value && value !== 'new' ? value : null
  const blank: FormState = { mode: 'url', url: '', protocol: 'socks5', host: '', port: null, username: '', password: '', nodeId: nodes[0]?.value, countryCode: 'us', city: '', provider: '', note: '', maxTenants: null }
  const [f, setF] = useState<FormState>(blank)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    if (!value) return
    setError(null)
    if (value === 'new') setF({ ...blank, nodeId: nodes[0]?.value, ...defaults })
    else
      setF({
        mode: 'fields',
        url: '',
        protocol: value.protocol,
        host: value.host,
        port: value.port,
        username: value.username,
        password: '',
        nodeId: value.nodeId,
        countryCode: value.countryCode,
        city: value.city ?? '',
        provider: value.provider,
        note: value.note,
        maxTenants: value.maxTenants,
      })
  }, [value])
  const set = <K extends keyof FormState>(k: K, v: FormState[K]) => setF((x) => ({ ...x, [k]: v }))
  const country = COUNTRIES.find((c) => c.value === f.countryCode)

  const submit = async () => {
    setBusy(true)
    setError(null)
    try {
      if (editing) {
        await api.adminUpdateUpstream({
          upstreamId: editing.id,
          body: {
            rowVersion: editing.rowVersion,
            protocol: f.protocol,
            host: f.host.trim(),
            port: f.port ?? undefined,
            username: f.username,
            password: f.password || undefined,
            countryCode: f.countryCode,
            country: country?.label,
            // 留空即清除城市（null）。
            city: f.city.trim() || null,
            provider: f.provider,
            note: f.note,
            maxTenants: f.maxTenants !== null && f.maxTenants !== editing.maxTenants ? f.maxTenants : undefined,
          },
        })
        message.success('已保存')
      } else {
        const created = await api.adminCreateUpstream({
          body: {
            ...(f.mode === 'url' ? { url: f.url.trim() } : { protocol: f.protocol, host: f.host.trim(), port: f.port ?? undefined, username: f.username, password: f.password || undefined }),
            nodeId: f.nodeId!,
            countryCode: f.countryCode!,
            country: country?.label ?? f.countryCode!,
            city: f.city.trim() || undefined,
            provider: f.provider || undefined,
            note: f.note || undefined,
            maxTenants: f.maxTenants ?? undefined,
          },
        })
        message.success('已录入，状态为“待验证”')
        onCreated?.(created)
      }
      void qc.invalidateQueries({ queryKey: qk.upstreams })
      onClose()
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }

  const connReady = f.mode === 'url' ? f.url.trim().length > 0 : f.host.trim().length > 0 && !!f.port
  const shared = editing?.occupancy.mode === 'shared' ? editing.occupancy : null
  const ready = connReady && !!f.nodeId && !!f.countryCode

  return (
    <Modal
      open={!!value}
      title={editing ? `编辑上游 ${editing.host}` : '录入上游'}
      onCancel={() => !busy && onClose()}
      onOk={submit}
      okText={editing ? '保存' : '录入'}
      cancelText="取消"
      okButtonProps={{ disabled: !ready, loading: busy }}
      destroyOnHidden
      width={640}
    >
      <div className="confirm-body">
        {editing?.status === 'bound' && <Callout tone="warn">资源已被订阅占用：只能改地区、供应商与备注；改连接信息要先为订阅换出口。</Callout>}
        {shared && editing?.status !== 'bound' && (
          <Callout tone="warn">
            资源上有共享占用 {shared.used}/{editing!.maxTenants}：改连接信息要先为其上的订阅换出口；绑定上限不能低于当前占用，也不能改为 1。
          </Callout>
        )}
        {editing && editing.status !== 'bound' && !shared && <Callout tone="info">修改协议、主机、端口、用户名或密码后，资源回到“待验证”，需重新验证才能售卖。</Callout>}
        {!editing && (
          <Segmented
            value={f.mode}
            onChange={(v) => set('mode', v as FormState['mode'])}
            options={[
              { value: 'url', label: '粘贴地址' },
              { value: 'fields', label: '逐项填写' },
            ]}
          />
        )}
        {f.mode === 'url' && !editing ? (
          <label className="field">
            <span className="field-label">上游地址</span>
            <Input aria-label="上游地址" className="mono" value={f.url} onChange={(e) => set('url', e.target.value)} placeholder="socks5://user:pass@gw.example.net:1080" />
          </label>
        ) : (
          <div className="form-grid">
            <label className="field">
              <span className="field-label">协议</span>
              <Select aria-label="协议" value={f.protocol} onChange={(v) => set('protocol', v)} options={['http', 'https', 'socks5'].map((x) => ({ value: x, label: x.toUpperCase() }))} />
            </label>
            <label className="field">
              <span className="field-label">端口</span>
              <InputNumber aria-label="端口" min={1} max={65535} value={f.port} onChange={(v) => set('port', v)} style={{ width: '100%' }} />
            </label>
            <label className="field full" style={{ marginTop: 12 }}>
              <span className="field-label">主机</span>
              <Input aria-label="主机" className="mono" value={f.host} onChange={(e) => set('host', e.target.value)} />
            </label>
            <label className="field" style={{ marginTop: 12 }}>
              <span className="field-label">用户名</span>
              <Input aria-label="用户名" value={f.username} onChange={(e) => set('username', e.target.value)} autoComplete="off" />
            </label>
            <label className="field" style={{ marginTop: 12 }}>
              <span className="field-label">密码{editing && <em>{editing.hasPassword ? '已设置，留空不改' : '未设置'}</em>}</span>
              <Input.Password aria-label="上游密码" value={f.password} onChange={(e) => set('password', e.target.value)} autoComplete="new-password" />
            </label>
          </div>
        )}
        <div className="form-grid">
          <label className="field">
            <span className="field-label">节点</span>
            <Select aria-label="节点" value={f.nodeId} onChange={(v) => set('nodeId', v)} options={nodes} disabled={!!editing} />
          </label>
          <label className="field">
            <span className="field-label">国家</span>
            <Select aria-label="国家" value={f.countryCode} onChange={(v) => set('countryCode', v)} options={COUNTRIES} />
          </label>
          <label className="field" style={{ marginTop: 12 }}>
            <span className="field-label">
              城市<em>可选</em>
            </span>
            <Input aria-label="城市" value={f.city} placeholder="留空 = 未知城市" onChange={(e) => set('city', e.target.value)} />
          </label>
          <label className="field" style={{ marginTop: 12 }}>
            <span className="field-label">供应商</span>
            <Input aria-label="供应商" value={f.provider} onChange={(e) => set('provider', e.target.value)} />
          </label>
          <label className="field full" style={{ marginTop: 12 }}>
            <span className="field-label">
              绑定上限<em>1 只给独享订阅；≥2 时可由多个共享订阅共用，空闲时也可整个分给一个独享订阅</em>
            </span>
            <InputNumber
              aria-label="绑定上限"
              min={1}
              max={100}
              precision={0}
              value={f.maxTenants}
              onChange={(v) => set('maxTenants', v)}
              placeholder="留空用默认值"
              style={{ width: '100%' }}
            />
          </label>
          <label className="field full" style={{ marginTop: 12 }}>
            <span className="field-label">备注</span>
            <Input aria-label="备注" value={f.note} onChange={(e) => set('note', e.target.value)} />
          </label>
        </div>
        {error && <Callout tone="stop">{error}</Callout>}
      </div>
    </Modal>
  )
}
