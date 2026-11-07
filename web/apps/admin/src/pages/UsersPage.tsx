// 用户列表：按邮箱搜索、按状态筛选；管理员可代建账号（发送设置密码邮件）。
import { UserAddOutlined } from '@ant-design/icons'
import { useQueryClient } from '@tanstack/react-query'
import { App, Button, Input } from 'antd'
import { useMemo, useState } from 'react'
import { Link, useNavigate } from 'react-router'
import type { AdminUser } from '@isp/api-client/admin'
import { Plate, fmtDate } from '@isp/ui'
import { qk, useApi } from '../app/api'
import { Ago, PageHead, State } from '../components/bits'
import { ConfirmAction } from '../components/ConfirmAction'
import { DataTable, str, type Column, type FilterDef } from '../components/DataTable'
import { userState } from '../lib/status'

export default function UsersPage() {
  const api = useApi()
  const [creating, setCreating] = useState(false)
  const filters = useMemo<FilterDef[]>(
    () => [
      { key: 'q', label: '搜索', type: 'search', placeholder: '邮箱或用户 ID' },
      {
        key: 'status',
        label: '状态',
        type: 'select',
        options: [
          { value: 'active', label: '正常' },
          { value: 'unverified', label: '邮箱未验证' },
          { value: 'banned', label: '已封禁' },
        ],
      },
    ],
    [],
  )
  const columns = useMemo<Column<AdminUser>[]>(
    () => [
      {
        key: 'email',
        title: '邮箱',
        fixedVisible: true,
        render: (_, u) => (
          <div className="cell-stack">
            <Link to={`/users/${u.id}`}>{u.email}</Link>
            <span className="muted mono">{u.id}</span>
          </div>
        ),
      },
      { key: 'state', title: '状态', render: (_, u) => <State view={userState(u)} /> },
      { key: 'subs', title: '订阅', align: 'right', render: (_, u) => (u.subscriptionCount ? <Link to={`/subscriptions?userId=${u.id}`}>{u.subscriptionCount}</Link> : 0) },
      { key: 'orders', title: '未完成订单', align: 'right', render: (_, u) => u.openOrderCount },
      { key: 'login', title: '最近登录', render: (_, u) => <Ago at={u.lastLoginAt} empty="从未登录" /> },
      { key: 'created', title: '注册', render: (_, u) => fmtDate(u.createdAt) },
      { key: 'pw', title: '已设密码', defaultHidden: true, render: (_, u) => (u.passwordSet ? '是' : '否') },
      { key: 'by', title: '代建人', defaultHidden: true, render: (_, u) => u.createdByAdmin?.email ?? '—' },
    ],
    [],
  )
  return (
    <>
      <PageHead
        title="用户"
        extra={
          <Button icon={<UserAddOutlined />} onClick={() => setCreating(true)}>
            新建用户
          </Button>
        }
      />
      <Plate>
        <DataTable<AdminUser>
          id="users"
          label="用户列表"
          columns={columns}
          filters={filters}
          queryKey={qk.users}
          rowKey={(u) => u.id}
          rowClassName={(u) => (u.status === 'banned' ? 'row-muted' : '')}
          fetchPage={({ filters: f, cursor, limit }) =>
            api.adminListUsers({ query: { q: str(f, 'q'), status: str(f, 'status') as 'active' | 'unverified' | 'banned' | undefined, cursor, limit } })
          }
        />
      </Plate>
      <CreateUserDialog open={creating} onClose={() => setCreating(false)} />
    </>
  )
}

function CreateUserDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const api = useApi()
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { message } = App.useApp()
  const [email, setEmail] = useState('')
  return (
    <ConfirmAction
      open={open}
      onClose={() => {
        setEmail('')
        onClose()
      }}
      tone="info"
      title="新建用户"
      okText="创建并发送邮件"
      ready={/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(email.trim())}
      impacts={[<>创建一个邮箱未验证、未设密码的账号。</>, <>向该邮箱发送“设置密码”邮件；用户设置密码即完成验证。</>]}
      onConfirm={() => api.adminCreateUser({ body: { email: email.trim() } })}
      onDone={(u) => {
        message.success('已创建')
        setEmail('')
        void qc.invalidateQueries({ queryKey: qk.users })
        navigate(`/users/${(u as AdminUser).id}`)
      }}
    >
      <label className="field">
        <span className="field-label">邮箱</span>
        <Input aria-label="新用户邮箱" type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="name@example.com" />
      </label>
    </ConfirmAction>
  )
}
