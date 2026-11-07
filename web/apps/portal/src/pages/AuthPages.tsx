import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Form, Input } from 'antd'
import { useEffect, useState, type ReactNode } from 'react'
import { Link, useLocation, useNavigate } from 'react-router'
import { api, errorText, qk } from '../app/api'
import { Brand, ProtoRibbon } from '../app/Shell'
import { Plate } from '@isp/ui'

function AuthFrame({ title, sub, children }: { title: string; sub?: ReactNode; children: ReactNode }) {
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

export function LoginPage() {
  const navigate = useNavigate()
  const location = useLocation()
  const qc = useQueryClient()
  const from = (location.state as { from?: string } | null)?.from ?? '/ips'
  const login = useMutation({
    mutationFn: (v: { email: string; password: string }) => api.login(v.email, v.password),
    onSuccess: (a) => {
      qc.setQueryData(qk.session, a)
      navigate(a.verified ? from : '/verify-email', { replace: true })
    },
  })
  return (
    <AuthFrame title="登录" sub="查看你的 IP、复制连接、续时长或买流量。">
      <Form layout="vertical" requiredMark={false} onFinish={(v) => login.mutate(v)}>
        <Form.Item name="email" label="邮箱" rules={[{ required: true, type: 'email', message: '请输入有效的邮箱地址' }]}>
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
  const qc = useQueryClient()
  const reg = useMutation({
    mutationFn: (v: { email: string; password: string }) => api.register(v.email, v.password),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: qk.session })
      navigate('/verify-email', { replace: true })
    },
  })
  return (
    <AuthFrame title="注册" sub="验证邮箱后才能购买或获取连接。">
      <Form layout="vertical" requiredMark={false} onFinish={(v) => reg.mutate(v)}>
        <Form.Item name="email" label="邮箱" rules={[{ required: true, type: 'email', message: '请输入有效的邮箱地址' }]}>
          <Input autoComplete="email" inputMode="email" placeholder="you@example.com" />
        </Form.Item>
        <Form.Item
          name="password"
          label="登录密码"
          extra="至少 10 位。登录密码与代理接入密码相互独立。"
          rules={[{ required: true, message: '请设置密码' }, { min: 10, message: '至少 10 位' }]}
        >
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

export function VerifyEmailPage() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const session = useQuery({ queryKey: qk.session, queryFn: () => api.session() })
  const [cooldown, setCooldown] = useState(60)
  useEffect(() => {
    if (cooldown <= 0) return
    const t = window.setTimeout(() => setCooldown((n) => n - 1), 1000)
    return () => window.clearTimeout(t)
  }, [cooldown])
  const verify = useMutation({
    mutationFn: (v: { code: string }) => api.verifyEmail(v.code),
    onSuccess: (a) => {
      qc.setQueryData(qk.session, a)
      navigate('/ips', { replace: true })
    },
  })
  const resend = useMutation({ mutationFn: () => api.resendVerification(), onSuccess: () => setCooldown(60) })
  return (
    <AuthFrame
      title="验证邮箱"
      sub={
        <>
          验证码已发送到 <b>{session.data?.email ?? '你的邮箱'}</b>，30 分钟内有效。原型中的验证码是 123456。
        </>
      }
    >
      <Form layout="vertical" requiredMark={false} onFinish={(v) => verify.mutate(v)}>
        <Form.Item name="code" label="验证码" rules={[{ required: true, len: 6, message: '请输入 6 位验证码' }]}>
          <Input autoComplete="one-time-code" inputMode="numeric" maxLength={6} className="mono" style={{ letterSpacing: '0.3em', fontSize: 18 }} />
        </Form.Item>
        {verify.isError && <Alert style={{ marginBottom: 16 }} type="error" showIcon title={errorText(verify.error)} />}
        <Button type="primary" htmlType="submit" block size="large" loading={verify.isPending}>
          验证
        </Button>
      </Form>
      <div className="auth-links" style={{ marginTop: 16 }}>
        <Button type="link" style={{ padding: 0 }} disabled={cooldown > 0} loading={resend.isPending} onClick={() => resend.mutate()}>
          {cooldown > 0 ? `${cooldown} 秒后可重发` : '重新发送验证码'}
        </Button>
        <span className="plate-note">重发后旧验证码失效</span>
      </div>
    </AuthFrame>
  )
}

export function ForgotPasswordPage() {
  const send = useMutation({ mutationFn: (v: { email: string }) => api.requestPasswordReset(v.email) })
  return (
    <AuthFrame title="找回密码" sub="输入注册邮箱，我们会发送一次性的重设链接。">
      {send.isSuccess ? (
        <Alert
          type="success"
          showIcon
          title="如果这个邮箱已注册，重设链接已经发出"
          description="链接 30 分钟内有效，只能使用一次。重设后其他设备上的登录会失效。"
        />
      ) : (
        <Form layout="vertical" requiredMark={false} onFinish={(v) => send.mutate(v)}>
          <Form.Item name="email" label="邮箱" rules={[{ required: true, type: 'email', message: '请输入有效的邮箱地址' }]}>
            <Input autoComplete="email" inputMode="email" placeholder="you@example.com" />
          </Form.Item>
          {send.isError && <Alert style={{ marginBottom: 16 }} type="error" showIcon title={errorText(send.error)} />}
          <Button type="primary" htmlType="submit" block size="large" loading={send.isPending}>
            发送重设链接
          </Button>
        </Form>
      )}
      <div className="auth-links" style={{ marginTop: 16 }}>
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
