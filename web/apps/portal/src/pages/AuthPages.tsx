import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Form, Input, type InputProps } from 'antd'
import { useEffect, useState, type ReactNode } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router'
import type { Account } from '@isp/api-client'
import { USE_MOCK, api, errorText, qk, setQueryUser } from '../app/api'
import { Brand, ProtoRibbon, useTitle } from '../app/Shell'
import { Plate } from '@isp/ui'

function AuthFrame({ title, sub, children }: { title: string; sub?: ReactNode; children: ReactNode }) {
  useTitle(title)
  return (
    <>
      <ProtoRibbon />
      <main className="auth-shell">
        <div className="auth-col">
          <Brand />
          <Plate>
            <div className="plate-pad" style={{ paddingTop: 26 }}>
              <h1>{title}</h1>
              {sub && <p className="auth-sub">{sub}</p>}
              {children}
            </div>
          </Plate>
        </div>
      </main>
    </>
  )
}

/** 登录后回到的站内路径：只接受以单个 / 开头的相对路径，防止跳到站外。 */
function safeNext(raw: string | null): string {
  if (!raw || !raw.startsWith('/') || raw.startsWith('//')) return '/ips'
  return raw
}

const EMAIL_RULES = [{ required: true, type: 'email' as const, message: '请输入有效的邮箱地址' }]
const CODE_RULES = [{ required: true, pattern: /^[0-9]{6}$/, message: '请输入 6 位数字验证码' }]
const NEW_PASSWORD_RULES = [{ required: true, message: '请设置密码' }, { min: 10, message: '至少 10 位' }]

/** 登录、验证邮箱成功后：记下当前用户再写入会话缓存（查询键随用户变化）。 */
function useSignedIn() {
  const qc = useQueryClient()
  return (a: Account) => {
    qc.clear()
    setQueryUser(a.email)
    qc.setQueryData(qk.session, a)
  }
}

/** Form.Item 会给直接子元素注入 id、value、onChange，这里原样转给 Input。 */
function CodeInput(props: InputProps) {
  return (
    <Input
      {...props}
      autoComplete="one-time-code"
      inputMode="numeric"
      maxLength={6}
      className="mono"
      style={{ letterSpacing: '0.3em', fontSize: 18 }}
    />
  )
}

export function LoginPage() {
  const navigate = useNavigate()
  const [params] = useSearchParams()
  const next = safeNext(params.get('next'))
  const expired = params.get('expired') === '1'
  const signedIn = useSignedIn()
  const login = useMutation({
    mutationFn: (v: { email: string; password: string }) => api.login(v.email, v.password),
    onSuccess: (a) => {
      signedIn(a)
      navigate(a.verified ? next : '/verify-email', { replace: true })
    },
  })
  return (
    <AuthFrame title="登录" sub="查看你的 IP、复制连接、续时长或买流量。">
      {expired && !login.isPending && (
        <Alert style={{ marginBottom: 16 }} type="info" showIcon title="登录已失效，请重新登录" description="登录后回到刚才的页面。" />
      )}
      <Form layout="vertical" requiredMark={false} onFinish={(v) => login.mutate(v)}>
        <Form.Item name="email" label="邮箱" rules={EMAIL_RULES}>
          <Input autoComplete="email" inputMode="email" placeholder="you@example.com" />
        </Form.Item>
        <Form.Item name="password" label="密码" rules={[{ required: true, message: '请输入密码' }]}>
          <Input.Password autoComplete="current-password" />
        </Form.Item>
        {login.isError && <Alert style={{ marginBottom: 16 }} type="error" showIcon title={errorText(login.error)} />}
        <Button type="primary" htmlType="submit" block size="large" loading={login.isPending}>
          登录
        </Button>
      </Form>
      <div className="auth-links" style={{ marginTop: 16 }}>
        <Link to="/register">注册新账号</Link>
        <Link to="/forgot-password">忘记密码</Link>
      </div>
    </AuthFrame>
  )
}

export function RegisterPage() {
  const navigate = useNavigate()
  // 注册不建会话（不泄露邮箱是否已注册）：带着邮箱去验证页，验证成功即登录。
  const reg = useMutation({
    mutationFn: (v: { email: string; password: string }) => api.register(v.email, v.password),
    onSuccess: (r) => navigate(`/verify-email?email=${encodeURIComponent(r.email)}`, { replace: true }),
  })
  return (
    <AuthFrame title="注册" sub="验证邮箱后才能购买或获取连接。">
      <Form layout="vertical" requiredMark={false} onFinish={(v) => reg.mutate(v)}>
        <Form.Item name="email" label="邮箱" rules={EMAIL_RULES}>
          <Input autoComplete="email" inputMode="email" placeholder="you@example.com" />
        </Form.Item>
        <Form.Item name="password" label="登录密码" extra="至少 10 位。登录密码与代理接入密码相互独立。" rules={NEW_PASSWORD_RULES}>
          <Input.Password autoComplete="new-password" />
        </Form.Item>
        {reg.isError && <Alert style={{ marginBottom: 16 }} type="error" showIcon title={errorText(reg.error)} />}
        <Button type="primary" htmlType="submit" block size="large" loading={reg.isPending}>
          注册并发送验证码
        </Button>
      </Form>
      <div className="auth-links" style={{ marginTop: 16 }}>
        <span className="plate-note">已有账号？</span>
        <Link to="/login">去登录</Link>
      </div>
    </AuthFrame>
  )
}

function useCooldown(initial: number) {
  const [cooldown, setCooldown] = useState(initial)
  useEffect(() => {
    if (cooldown <= 0) return
    const t = window.setTimeout(() => setCooldown((n) => n - 1), 1000)
    return () => window.clearTimeout(t)
  }, [cooldown])
  return [cooldown, setCooldown] as const
}

export function VerifyEmailPage() {
  const navigate = useNavigate()
  const [params] = useSearchParams()
  const signedIn = useSignedIn()
  const session = useQuery({ queryKey: qk.session, queryFn: () => api.session() })
  // 已登录（未验证）时验证本人邮箱；未登录时（刚注册）按邮箱验证，成功即登录。
  const loggedIn = session.data ?? null
  const [email, setEmail] = useState(params.get('email') ?? '')
  const [cooldown, setCooldown] = useCooldown(60)
  useEffect(() => {
    if (loggedIn?.verified) navigate('/ips', { replace: true })
  }, [loggedIn, navigate])
  const verify = useMutation({
    mutationFn: (v: { code: string; email?: string }) => api.verifyEmail(v.code.trim(), loggedIn ? undefined : v.email?.trim()),
    onSuccess: (a) => {
      signedIn(a)
      navigate('/ips', { replace: true })
    },
  })
  const resend = useMutation({
    mutationFn: () => api.resendVerification(loggedIn ? undefined : email.trim() || undefined),
    onSuccess: () => setCooldown(60),
  })
  const target = loggedIn?.email ?? (email || '你的邮箱')
  return (
    <AuthFrame
      title="验证邮箱"
      sub={
        <>
          验证码已发送到 <b>{target}</b>，15 分钟内有效。
          {USE_MOCK && ' 演示模式的验证码是 123456。'}
        </>
      }
    >
      <Form<{ code: string; email?: string }>
        layout="vertical"
        requiredMark={false}
        initialValues={{ email }}
        onValuesChange={(c: { email?: string }) => c.email !== undefined && setEmail(c.email)}
        onFinish={(v) => verify.mutate(v)}
      >
        {!loggedIn && !session.isPending && (
          <Form.Item name="email" label="注册邮箱" rules={EMAIL_RULES}>
            <Input autoComplete="email" inputMode="email" placeholder="you@example.com" />
          </Form.Item>
        )}
        <Form.Item name="code" label="验证码" rules={CODE_RULES}>
          <CodeInput />
        </Form.Item>
        {verify.isError && <Alert style={{ marginBottom: 16 }} type="error" showIcon title={errorText(verify.error)} />}
        <Button type="primary" htmlType="submit" block size="large" loading={verify.isPending}>
          验证并登录
        </Button>
      </Form>
      <div className="auth-links" style={{ marginTop: 16 }}>
        <Button
          type="link"
          style={{ padding: 0 }}
          disabled={cooldown > 0 || (!loggedIn && !email)}
          loading={resend.isPending}
          onClick={() => resend.mutate()}
        >
          {cooldown > 0 ? `${cooldown} 秒后可重发` : '重新发送验证码'}
        </Button>
        <span className="plate-note">重发后旧验证码失效</span>
      </div>
      {resend.isError && <Alert style={{ marginTop: 12 }} type="error" showIcon title={errorText(resend.error)} />}
      {!loggedIn && (
        <div className="auth-links" style={{ marginTop: 8 }}>
          <Link to="/login">返回登录</Link>
        </div>
      )}
    </AuthFrame>
  )
}

export function ForgotPasswordPage() {
  const navigate = useNavigate()
  const send = useMutation({
    mutationFn: (v: { email: string }) => api.requestPasswordReset(v.email.trim()),
    onSuccess: (_, v) => navigate(`/reset-password?email=${encodeURIComponent(v.email.trim())}&sent=1`),
  })
  return (
    <AuthFrame title="找回密码" sub="输入注册邮箱，我们会发送一次性的验证码，用它设置新密码。">
      <Form layout="vertical" requiredMark={false} onFinish={(v) => send.mutate(v)}>
        <Form.Item name="email" label="邮箱" rules={EMAIL_RULES}>
          <Input autoComplete="email" inputMode="email" placeholder="you@example.com" />
        </Form.Item>
        {send.isError && <Alert style={{ marginBottom: 16 }} type="error" showIcon title={errorText(send.error)} />}
        <Button type="primary" htmlType="submit" block size="large" loading={send.isPending}>
          发送验证码
        </Button>
      </Form>
      <div className="auth-links" style={{ marginTop: 16 }}>
        <Link to="/reset-password">已有验证码</Link>
        <Link to="/login">返回登录</Link>
      </div>
    </AuthFrame>
  )
}

interface ResetValues {
  email: string
  code: string
  password: string
  confirm: string
}

/** 用邮件里的验证码设置新密码（找回密码；管理员代建的账号首次设置密码也走这里）。 */
export function ResetPasswordPage() {
  const [params] = useSearchParams()
  const navigate = useNavigate()
  const qc = useQueryClient()
  const initial = { email: params.get('email') ?? '', code: params.get('code') ?? '' }
  const sent = params.get('sent') === '1'
  const confirm = useMutation({
    mutationFn: (v: ResetValues) => api.confirmPasswordReset(v.email.trim(), v.code.trim(), v.password),
    // 服务端撤销该账号全部会话：本地缓存一并清掉。
    onSuccess: () => {
      qc.clear()
      setQueryUser(null)
    },
  })
  if (confirm.isSuccess) {
    return (
      <AuthFrame title="新密码已设置">
        <Alert type="success" showIcon title="请用新密码登录" description="所有设备上的登录都已失效；代理连接不受影响。" />
        <Button type="primary" block size="large" style={{ marginTop: 16 }} onClick={() => navigate('/login', { replace: true })}>
          去登录
        </Button>
      </AuthFrame>
    )
  }
  return (
    <AuthFrame
      title="设置新密码"
      sub={sent ? '如果这个邮箱已注册，验证码已经发出，15 分钟内有效、只能使用一次。' : '输入邮件里的 6 位验证码和新密码。'}
    >
      <Form<ResetValues> layout="vertical" requiredMark={false} initialValues={initial} onFinish={(v) => confirm.mutate(v)}>
        <Form.Item name="email" label="邮箱" rules={EMAIL_RULES}>
          <Input autoComplete="email" inputMode="email" placeholder="you@example.com" />
        </Form.Item>
        <Form.Item name="code" label="验证码" rules={CODE_RULES}>
          <CodeInput />
        </Form.Item>
        <Form.Item name="password" label="新密码" extra="至少 10 位。" rules={NEW_PASSWORD_RULES}>
          <Input.Password autoComplete="new-password" />
        </Form.Item>
        <Form.Item
          name="confirm"
          label="再次输入新密码"
          dependencies={['password']}
          rules={[
            { required: true, message: '请再输入一次' },
            ({ getFieldValue }) => ({
              validator: (_, v) => (v && v !== getFieldValue('password') ? Promise.reject(new Error('两次输入不一致')) : Promise.resolve()),
            }),
          ]}
        >
          <Input.Password autoComplete="new-password" />
        </Form.Item>
        {confirm.isError && <Alert style={{ marginBottom: 16 }} type="error" showIcon title={errorText(confirm.error)} />}
        <Button type="primary" htmlType="submit" block size="large" loading={confirm.isPending}>
          设置新密码
        </Button>
      </Form>
      <div className="auth-links" style={{ marginTop: 16 }}>
        <Link to="/forgot-password">重新获取验证码</Link>
        <Link to="/login">返回登录</Link>
      </div>
    </AuthFrame>
  )
}

export function NotFoundPage() {
  return (
    <div className="empty">
      <h2>页面不存在</h2>
      <p>
        <Link to="/ips">返回我的 IP</Link>
      </p>
    </div>
  )
}
