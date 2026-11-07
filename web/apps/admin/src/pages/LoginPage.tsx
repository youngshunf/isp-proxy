// 管理员登录（docs/06 §3.2）：邮箱 + 密码 → 第二步验证码（TOTP）或恢复码。
// 首次登录必须先绑定验证器：显示密钥与二维码，输入一次验证码完成绑定，然后一次性展示恢复码。
import { KeyOutlined, LockOutlined, SafetyOutlined } from '@ant-design/icons'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Checkbox, Input, QRCode } from 'antd'
import { useState, type FormEvent } from 'react'
import { useNavigate, useSearchParams } from 'react-router'
import type { AdminLoginChallenge, AdminSession } from '@isp/api-client/admin'
import { Plate } from '@isp/ui'
import { errorText, qk, useApi, USE_MOCK } from '../app/api'
import { Brand, ProtoRibbon } from '../app/Shell'
import { Callout, CopyButton, Mono } from '../components/bits'

type Stage = { kind: 'password' } | { kind: 'mfa'; challenge: AdminLoginChallenge } | { kind: 'codes'; session: AdminSession }

export default function LoginPage() {
  const api = useApi()
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [sp] = useSearchParams()
  const expired = sp.get('expired') === '1'
  const from = safeFrom(sp.get('from'))

  const [stage, setStage] = useState<Stage>({ kind: 'password' })
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [code, setCode] = useState('')
  const [useRecovery, setUseRecovery] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [saved, setSaved] = useState(false)

  const finish = (s: AdminSession) => {
    qc.setQueryData(qk.me, s.admin)
    navigate(from, { replace: true })
  }

  const submitPassword = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setError(null)
    try {
      const challenge = await api.adminLogin({ body: { email: email.trim(), password } })
      setCode('')
      setStage({ kind: 'mfa', challenge })
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  const submitCode = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setError(null)
    try {
      const s = await api.adminVerifyMfa({ body: useRecovery ? { recoveryCode: code.trim() } : { code: code.trim() } })
      if (s.recoveryCodes?.length) setStage({ kind: 'codes', session: s })
      else finish(s)
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  const enrollment = stage.kind === 'mfa' && stage.challenge.stage === 'enrollment_required' ? stage.challenge.enrollment : undefined

  return (
    <div className="login">
      <ProtoRibbon />
      <main className="login-body">
        <div className="login-card">
          <Brand />
          <Plate>
            <div className="plate-pad">
              {stage.kind === 'password' && (
                <>
                  <h1>登录管理后台</h1>
                  <p className="muted small">仅限运营人员。登录需要密码和验证器两步。</p>
                  {expired && (
                    <Callout tone="warn" icon={<LockOutlined />}>
                      会话已过期或已在别处退出，请重新登录。登录后回到刚才的页面。
                    </Callout>
                  )}
                  <form onSubmit={submitPassword} noValidate>
                    <label className="field">
                      <span className="field-label">邮箱</span>
                      <Input aria-label="邮箱" type="email" autoComplete="username" value={email} onChange={(e) => setEmail(e.target.value)} autoFocus />
                    </label>
                    <label className="field">
                      <span className="field-label">密码</span>
                      <Input.Password aria-label="密码" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />
                    </label>
                    {error && <Callout tone="stop">{error}</Callout>}
                    <Button type="primary" htmlType="submit" loading={busy} disabled={!email.trim() || !password} block>
                      下一步
                    </Button>
                  </form>
                  {USE_MOCK && (
                    <p className="muted small" style={{ marginTop: 14 }}>
                      原型账号：<Mono>ops@example.com</Mono> / <Mono>admin-pass-1</Mono>，验证码 <Mono>246810</Mono>；只读账号 <Mono>audit@example.com</Mono> /{' '}
                      <Mono>viewer-pass-1</Mono>；首次绑定演示 <Mono>new-admin@example.com</Mono>。
                    </p>
                  )}
                </>
              )}

              {stage.kind === 'mfa' && (
                <>
                  <h1>{enrollment ? '绑定验证器' : '输入验证码'}</h1>
                  <p className="muted small">{email}</p>
                  {enrollment && (
                    <div className="stack" style={{ gap: 12, marginTop: 14 }}>
                      <p className="small">首次登录需要绑定验证器（Google Authenticator、1Password 等支持 TOTP 的应用）：扫描二维码，或手动输入密钥。</p>
                      <div className="enroll">
                        <QRCode value={enrollment.otpauthUri} size={136} bordered={false} type="svg" aria-label="验证器绑定二维码" />
                        <div className="stack" style={{ gap: 6 }}>
                          <span className="field-label">密钥</span>
                          <span>
                            <Mono>{enrollment.secret.replace(/(.{4})/g, '$1 ').trim()}</Mono>
                            <CopyButton text={enrollment.secret} label="复制密钥" />
                          </span>
                          <span className="muted small">类型 TOTP · 6 位 · 30 秒。密钥只在这里显示一次。</span>
                        </div>
                      </div>
                    </div>
                  )}
                  <form onSubmit={submitCode} noValidate>
                    <label className="field">
                      <span className="field-label">{useRecovery ? '恢复码' : '验证码'}</span>
                      {useRecovery ? (
                        <Input aria-label="恢复码" className="otp-input" value={code} onChange={(e) => setCode(e.target.value)} autoComplete="one-time-code" autoFocus />
                      ) : (
                        <Input
                          aria-label="验证码"
                          className="otp-input"
                          inputMode="numeric"
                          maxLength={6}
                          value={code}
                          onChange={(e) => setCode(e.target.value.replace(/\D/g, ''))}
                          autoComplete="one-time-code"
                          autoFocus
                          placeholder="6 位数字"
                        />
                      )}
                    </label>
                    {error && <Callout tone="stop">{error}</Callout>}
                    <Button
                      type="primary"
                      htmlType="submit"
                      loading={busy}
                      disabled={useRecovery ? !code.trim() : code.length !== 6}
                      block
                      icon={enrollment ? <SafetyOutlined /> : undefined}
                    >
                      {enrollment ? '绑定并登录' : '登录'}
                    </Button>
                    <div className="actions" style={{ justifyContent: 'space-between' }}>
                      <Button
                        type="link"
                        size="small"
                        onClick={() => {
                          setStage({ kind: 'password' })
                          setError(null)
                          setUseRecovery(false)
                        }}
                      >
                        换个账号
                      </Button>
                      {!enrollment && (
                        <Button
                          type="link"
                          size="small"
                          icon={<KeyOutlined />}
                          onClick={() => {
                            setUseRecovery((x) => !x)
                            setCode('')
                            setError(null)
                          }}
                        >
                          {useRecovery ? '用验证码' : '验证器不在身边？用恢复码'}
                        </Button>
                      )}
                    </div>
                  </form>
                </>
              )}

              {stage.kind === 'codes' && (
                <>
                  <h1>保存恢复码</h1>
                  <p className="small" style={{ margin: '6px 0 14px' }}>
                    验证器丢失时，每个恢复码可以代替验证码登录一次。<b>只显示这一次</b>，请抄写或存进密码管理器。
                  </p>
                  <ol className="codes" aria-label="恢复码">
                    {stage.session.recoveryCodes!.map((c) => (
                      <li key={c}>{c}</li>
                    ))}
                  </ol>
                  <div className="actions" style={{ margin: '12px 0' }}>
                    <CopyButton text={stage.session.recoveryCodes!.join('\n')} label="复制全部恢复码" />
                  </div>
                  <form
                    onSubmit={(e) => {
                      e.preventDefault()
                      finish(stage.session)
                    }}
                  >
                    <Checkbox checked={saved} onChange={(e) => setSaved(e.target.checked)}>
                      我已保存这些恢复码
                    </Checkbox>
                    <Button type="primary" htmlType="submit" disabled={!saved} block>
                      进入后台
                    </Button>
                  </form>
                </>
              )}
            </div>
          </Plate>
        </div>
      </main>
    </div>
  )
}

/** 只接受站内路径，防止 ?from= 被拿来做开放跳转。 */
function safeFrom(v: string | null): string {
  if (!v || !v.startsWith('/') || v.startsWith('//') || v.startsWith('/login')) return '/'
  return v
}
