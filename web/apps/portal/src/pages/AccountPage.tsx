import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, App, Button, Form, Input } from 'antd'
import { useNavigate } from 'react-router'
import { api, errorText, qk } from '../app/api'
import { StateMark } from '@isp/ui'
import { Plate } from '@isp/ui'
import { TZ_LABEL, fmtDate } from '../lib/format'

interface PwValues {
  current: string
  next: string
  confirm: string
}

export default function AccountPage() {
  const session = useQuery({ queryKey: qk.session, queryFn: () => api.session() })
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { message } = App.useApp()
  const [form] = Form.useForm<PwValues>()
  const change = useMutation({
    mutationFn: (v: PwValues) => api.changePassword(v.current, v.next),
    onSuccess: async () => {
      message.success('登录密码已修改，请重新登录')
      await api.logout()
      qc.clear()
      navigate('/login', { replace: true })
    },
  })
  const a = session.data

  return (
    <div className="narrow">
      <div className="page-head">
        <div>
          <h1>账号</h1>
          <div className="page-head-sub">登录密码与各 IP 的接入密码相互独立。</div>
        </div>
      </div>
      <div className="detail-col">
        <Plate title="登录信息" labelledBy="acct-h">
          <div className="plate-pad">
            <dl className="kv">
              <dt>邮箱</dt>
              <dd className="value">{a?.email}</dd>
              <dd className="act">
                <span className="count" style={{ color: 'var(--ok)' }}>
                  <StateMark shape="filled" />
                  <span style={{ color: 'var(--ink-2)' }}>已验证</span>
                </span>
              </dd>
              <dt>注册时间</dt>
              <dd className="value">{fmtDate(a?.createdAt)}</dd>
              <dd className="act" />
              <dt>时区</dt>
              <dd className="value">{TZ_LABEL}</dd>
              <dd className="act" />
            </dl>
            <p className="plate-note" style={{ marginBottom: 0 }}>
              首期不支持自助更换邮箱或合并账号，如需处理请联系客服。
            </p>
          </div>
        </Plate>

        <Plate title="修改登录密码" labelledBy="pw-h">
          <div className="plate-pad">
            <Form<PwValues> form={form} layout="vertical" requiredMark={false} onFinish={(v) => change.mutate(v)} style={{ maxWidth: 420 }}>
              <Form.Item name="current" label="当前密码" rules={[{ required: true, message: '请输入当前密码' }]}>
                <Input.Password autoComplete="current-password" />
              </Form.Item>
              <Form.Item name="next" label="新密码" rules={[{ required: true, message: '请输入新密码' }, { min: 10, message: '至少 10 位' }]}>
                <Input.Password autoComplete="new-password" />
              </Form.Item>
              <Form.Item
                name="confirm"
                label="再次输入新密码"
                dependencies={['next']}
                rules={[
                  { required: true, message: '请再输入一次' },
                  ({ getFieldValue }) => ({
                    validator: (_, v) => (v && v !== getFieldValue('next') ? Promise.reject(new Error('两次输入不一致')) : Promise.resolve()),
                  }),
                ]}
              >
                <Input.Password autoComplete="new-password" />
              </Form.Item>
              {change.isError && <Alert style={{ marginBottom: 16 }} type="error" showIcon title={errorText(change.error)} />}
              <Button type="primary" htmlType="submit" loading={change.isPending}>
                修改密码
              </Button>
              <p className="plate-note" style={{ margin: '12px 0 0' }}>
                修改后其他设备上的登录会失效；代理连接不受影响。
              </p>
            </Form>
          </div>
        </Plate>
      </div>
    </div>
  )
}
