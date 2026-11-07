import { CopyOutlined, EyeInvisibleOutlined, EyeOutlined, KeyOutlined, SwapOutlined, WarningOutlined } from '@ant-design/icons'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Alert, App, Button, Modal, Radio } from 'antd'
import { useEffect, useRef, useState } from 'react'
import { PROTOCOLS, connectionString, type Access, type Subscription } from '@isp/api-client'
import { api, errorText, qk } from '../app/api'
import { fmtDate } from '../lib/format'
import { Plate } from '@isp/ui'
import { PROTO_LABEL } from './ProtocolCopy'
import { useCopy } from './useCopy'

type Mode = 'password' | 'ports_and_password'

const MASK = '••••••••'

function masked(a: Access, proto: (typeof PROTOCOLS)[number]) {
  return connectionString({ ...a, password: 'PASSWORD' }, proto).replace('PASSWORD', '••••••')
}

/** 换接入确认框。破坏性操作，用模态框锁定焦点并写明影响。 */
function ChangeAccessDialog({
  sub,
  mode,
  onMode,
  onClose,
  onDone,
}: {
  sub: Subscription
  mode: Mode
  onMode: (m: Mode) => void
  onClose: () => void
  onDone: (a: Access) => void
}) {
  const qc = useQueryClient()
  const change = useMutation({
    mutationFn: () => api.changeAccess(sub.id, mode),
    onSuccess: (a) => {
      qc.setQueryData<Subscription>(qk.sub(sub.id), (old) => (old ? { ...old, access: a } : old))
      void qc.invalidateQueries({ queryKey: qk.subs })
      onDone(a)
    },
  })
  const left = sub.quotas.accessChangesLeft
  return (
    <Modal
      open
      title={`换接入 · ${sub.name}`}
      okText={mode === 'password' ? '确认重置密码' : '确认更换端口与密码'}
      cancelText="取消"
      onOk={() => change.mutate()}
      onCancel={onClose}
      okButtonProps={{ loading: change.isPending, disabled: left <= 0 }}
      mask={{ closable: !change.isPending }}
      width={520}
    >
      <Radio.Group value={mode} onChange={(e) => onMode(e.target.value as Mode)} style={{ width: '100%' }}>
        <div style={{ display: 'grid', gap: 10, margin: '8px 0 16px' }}>
          <Radio value="password">
            <b>重置密码</b>
            <div className="plate-note">端口不变，只换密码。适合只有密码泄露。</div>
          </Radio>
          <Radio value="ports_and_password">
            <b>更换端口与密码</b>
            <div className="plate-note">端口和密码一起换。适合整条连接串泄露或被他人使用。</div>
          </Radio>
        </div>
      </Radio.Group>
      <div className="callout stop">
        <WarningOutlined />
        <div>
          <b>旧连接会立即失效，正在使用的程序需要更新配置。</b>
          <div>
            出口 <span className="mono">{sub.exit?.ip ?? '—'}</span>、剩余时长和流量都不变。
          </div>
        </div>
      </div>
      <p className="plate-note" style={{ margin: '12px 0 0' }}>
        24 小时内还可换 {left} / {sub.quotas.accessChangesPerDay} 次。
      </p>
      {left <= 0 && <Alert style={{ marginTop: 12 }} type="warning" showIcon title="次数已用完，请稍后再试或联系客服。" />}
      {change.isError && <Alert style={{ marginTop: 12 }} type="error" showIcon title={errorText(change.error)} />}
    </Modal>
  )
}

/** 接入分区：服务器、端口、凭据与三种连接串；换接入后高亮被改动的字段，直到用户确认已更新程序。 */
export function AccessPanel({ sub }: { sub: Subscription }) {
  const access = sub.access
  const { message } = App.useApp()
  const qc = useQueryClient()
  const { copied, announce, copy } = useCopy()
  const [reveal, setReveal] = useState(false)
  const [dialog, setDialog] = useState<Mode | null>(null)
  const ackRef = useRef<HTMLDivElement>(null)
  const ack = useMutation({
    mutationFn: () => api.acknowledgeAccess(sub.id),
    onSuccess: () => {
      qc.setQueryData<Subscription>(qk.sub(sub.id), (old) =>
        old?.access ? { ...old, access: { ...old.access, unseenChanges: [] } } : old,
      )
      void qc.invalidateQueries({ queryKey: qk.subs })
    },
  })
  const changed = new Set(access?.unseenChanges ?? [])
  const justChanged = useRef(false)
  useEffect(() => {
    if (justChanged.current && changed.size > 0) {
      ackRef.current?.focus()
      justChanged.current = false
    }
  })

  if (!access) {
    return (
      <Plate title="接入" labelledBy="access-h">
        <div className="plate-pad plate-note">开通完成后在这里显示服务器、端口和凭据。</div>
      </Plate>
    )
  }

  const blockedSelf = sub.blockers.includes('admin_paused') || sub.blockers.includes('account_banned')
  const fieldLabel = [changed.has('ports') && '端口', changed.has('password') && '密码'].filter(Boolean).join('与')

  return (
    <Plate
      title="接入"
      labelledBy="access-h"
      extra={
        <span className="plate-note">
          v{access.version} · 签发于 {fmtDate(access.issuedAt)}
        </span>
      }
    >
      {changed.size > 0 && (
        <div className="ack-bar" ref={ackRef} tabIndex={-1} role="status">
          <span>
            接入已更新到 v{access.version}：{fieldLabel}已更换，旧连接已失效。复制新连接、更新程序后点确认。
          </span>
          <Button type="primary" loading={ack.isPending} onClick={() => ack.mutate()}>
            我已更新程序
          </Button>
        </div>
      )}
      <div className="plate-pad" style={{ paddingTop: changed.size > 0 ? 0 : undefined }}>
        <dl className="kv">
          <dt>服务器</dt>
          <dd className="value mono">{access.host}</dd>
          <dd className="act">
            <Button size="small" type="text" icon={<CopyOutlined />} onClick={() => copy('host', access.host, '服务器地址')}>
              {copied === 'host' ? '已复制' : '复制'}
            </Button>
          </dd>

          <dt>端口</dt>
          <dd className="value">
            <span>
            <span className={changed.has('ports') ? 'changed' : undefined}>
              {PROTOCOLS.map((p, i) => (
                <span key={p}>
                  {i > 0 && <span style={{ color: 'var(--line)' }}> · </span>}
                  {PROTO_LABEL[p]} <span className="mono">{access.ports[p]}</span>
                </span>
              ))}
            </span>
            {changed.has('ports') && <span className="new-tag">新</span>}
            </span>
          </dd>
          <dd className="act" />

          <dt>用户名</dt>
          <dd className="value mono">{access.username}</dd>
          <dd className="act">
            <Button size="small" type="text" icon={<CopyOutlined />} onClick={() => copy('user', access.username, '用户名')}>
              {copied === 'user' ? '已复制' : '复制'}
            </Button>
          </dd>

          <dt>密码</dt>
          <dd className="value">
            <span>
              <span className={`mono${changed.has('password') ? ' changed' : ''}`}>{reveal ? access.password : MASK}</span>
              {changed.has('password') && <span className="new-tag">新</span>}
            </span>
          </dd>
          <dd className="act" style={{ whiteSpace: 'nowrap' }}>
            <Button
              size="small"
              type="text"
              icon={reveal ? <EyeInvisibleOutlined /> : <EyeOutlined />}
              onClick={() => setReveal((r) => !r)}
              aria-pressed={reveal}
            >
              {reveal ? '隐藏' : '显示'}
            </Button>
            <Button size="small" type="text" icon={<CopyOutlined />} onClick={() => copy('pass', access.password, '密码')}>
              {copied === 'pass' ? '已复制' : '复制'}
            </Button>
          </dd>
        </dl>

        <div className="conn-list" style={{ marginTop: 14 }}>
          {PROTOCOLS.map((p) => (
            <div className="conn-row" key={p}>
              <span className="conn-proto">{PROTO_LABEL[p]}</span>
              <span className="conn-str" title={reveal ? connectionString(access, p) : undefined}>
                {reveal ? connectionString(access, p) : masked(access, p)}
              </span>
              <Button
                size="small"
                type={copied === p ? 'primary' : 'default'}
                icon={<CopyOutlined />}
                onClick={() => copy(p, connectionString(access, p), ` ${PROTO_LABEL[p]} 连接串`)}
              >
                {copied === p ? '已复制' : '复制'}
              </Button>
            </div>
          ))}
        </div>
        <span className="visually-hidden" aria-live="polite">
          {announce}
        </span>

        <div className="actions" style={{ marginTop: 18, alignItems: 'center' }}>
          <Button icon={<KeyOutlined />} onClick={() => setDialog('password')} disabled={blockedSelf}>
            重置密码
          </Button>
          <Button icon={<SwapOutlined />} onClick={() => setDialog('ports_and_password')} disabled={blockedSelf}>
            更换接入
          </Button>
          <span className="plate-note">
            {blockedSelf
              ? '当前状态不能自助更换，请联系客服'
              : `24 小时内还可换 ${sub.quotas.accessChangesLeft} / ${sub.quotas.accessChangesPerDay} 次 · 只换接入，出口与余量不变`}
          </span>
        </div>
      </div>

      {dialog && (
        <ChangeAccessDialog
          sub={sub}
          mode={dialog}
          onMode={setDialog}
          onClose={() => setDialog(null)}
          onDone={(a) => {
            setDialog(null)
            setReveal(true)
            justChanged.current = true
            void qc.invalidateQueries({ queryKey: qk.events(sub.id) })
            message.success(`接入已更新到 v${a.version}，请复制新连接`)
          }}
        />
      )}
    </Plate>
  )
}
