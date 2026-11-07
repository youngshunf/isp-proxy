// 用户详情：账号信息、名下订阅与最近订单；封禁 / 解封、发送重置密码邮件。
// 封禁前在确认框里逐条列出受影响的订阅与会话，原因必填。
import { MailOutlined, StopOutlined, UnlockOutlined } from '@ant-design/icons'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { App, Button, Table } from 'antd'
import { useState } from 'react'
import { Link, useParams } from 'react-router'
import type { AdminUserDetail } from '@isp/api-client/admin'
import { Plate, fmtDateTime, fmtMoney } from '@isp/ui'
import { qk, useApi } from '../app/api'
import { Ago, Callout, Cell, Facts, LoadError, Loading, Mono, PageHead, State } from '../components/bits'
import { ConfirmAction } from '../components/ConfirmAction'
import { subColumns } from '../components/subCells'
import { FULFILLMENT, ORDER_KIND, PAYMENT, userState } from '../lib/status'

export default function UserDetailPage() {
  const { id = '' } = useParams()
  const api = useApi()
  const q = useQuery({ queryKey: qk.user(id), queryFn: () => api.adminGetUser({ userId: id }) })
  const [dlg, setDlg] = useState<'ban' | 'unban' | 'reset' | null>(null)
  if (q.isPending) return <Loading rows={8} />
  if (q.isError) return <LoadError error={q.error} retry={() => void q.refetch()} />
  const u = q.data
  return (
    <>
      <PageHead
        back={{ to: '/users', label: '用户' }}
        title={
          <>
            {u.email}
            <State view={userState(u)} />
          </>
        }
        sub={
          <>
            <Mono>{u.id}</Mono>
            <span>注册 {fmtDateTime(u.createdAt)}</span>
            <span>
              最近登录 <Ago at={u.lastLoginAt} empty="从未登录" />
            </span>
          </>
        }
        extra={
          <>
            <Button icon={<MailOutlined />} onClick={() => setDlg('reset')} disabled={u.status === 'banned'}>
              发送重置密码邮件
            </Button>
            {u.status === 'banned' ? (
              <Button icon={<UnlockOutlined />} onClick={() => setDlg('unban')}>
                解除封禁
              </Button>
            ) : (
              <Button danger icon={<StopOutlined />} onClick={() => setDlg('ban')}>
                封禁
              </Button>
            )}
          </>
        }
      />
      <div className="stack">
        {u.status === 'banned' && (
          <Callout tone="stop" icon={<StopOutlined />}>
            {fmtDateTime(u.bannedAt)} 封禁：{u.bannedReason}
          </Callout>
        )}
        <Plate title="账号">
          <div className="plate-pad">
            <Facts
              cols={2}
              items={[
                ['邮箱验证', u.verified ? '已验证' : '未验证'],
                ['密码', u.passwordSet ? '已设置' : '未设置（等待用户设置）'],
                ['活动会话', u.activeSessions],
                ['订阅数', u.subscriptionCount],
                ['未完成订单', u.openOrderCount],
                ['代建人', u.createdByAdmin?.email ?? null],
              ]}
            />
          </div>
        </Plate>
        <Plate title={`订阅（${u.subscriptions.length}）`}>
          <Table
            className="plain-table"
            size="small"
            rowKey="id"
            pagination={false}
            scroll={{ x: 'max-content' }}
            dataSource={u.subscriptions}
            columns={subColumns().filter((c) => !c.defaultHidden)}
            locale={{ emptyText: '没有订阅' }}
            style={{ marginTop: 12 }}
          />
        </Plate>
        <Plate title="最近订单" extra={<Link to={`/orders?q=${encodeURIComponent(u.email)}`} className="small">全部订单</Link>}>
          <Table
            className="plain-table"
            size="small"
            rowKey="id"
            pagination={false}
            scroll={{ x: 'max-content' }}
            dataSource={u.recentOrders}
            locale={{ emptyText: '没有订单' }}
            style={{ marginTop: 12 }}
            columns={[
              { title: '订单号', dataIndex: 'no', render: (v: string, o) => <Link to={`/orders/${o.id}`}><Mono>{v}</Mono></Link> },
              { title: '类型', dataIndex: 'kind', render: (v: keyof typeof ORDER_KIND) => ORDER_KIND[v] },
              { title: '金额', dataIndex: 'amount', align: 'right', render: (v) => fmtMoney(v) },
              {
                title: '状态',
                render: (_, o) => (
                  <span className="cells">
                    <Cell k="收款" view={PAYMENT[o.payment]} />
                    <Cell k="履约" view={FULFILLMENT[o.fulfillment]} />
                  </span>
                ),
              },
              { title: '下单', dataIndex: 'createdAt', render: (v: string) => fmtDateTime(v) },
            ]}
          />
        </Plate>
      </div>
      <BanDialog user={u} open={dlg === 'ban'} onClose={() => setDlg(null)} />
      <UnbanDialog user={u} open={dlg === 'unban'} onClose={() => setDlg(null)} />
      <ResetDialog user={u} open={dlg === 'reset'} onClose={() => setDlg(null)} />
    </>
  )
}

function BanDialog({ user, open, onClose }: { user: AdminUserDetail; open: boolean; onClose: () => void }) {
  const api = useApi()
  const qc = useQueryClient()
  const { message } = App.useApp()
  const live = user.subscriptions.filter((s) => s.lifecycle !== 'deleted' && s.lifecycle !== 'released')
  return (
    <ConfirmAction
      open={open}
      onClose={onClose}
      title={`封禁用户 ${user.email}`}
      okText="封禁"
      reasonLabel="封禁原因"
      impacts={[
        <>
          立即撤销 <b>{user.activeSessions}</b> 个登录会话，账号不能再登录。
        </>,
        live.length ? (
          <>
            名下 <b>{live.length}</b> 个订阅立即停止转发（现有连接 1 秒内断开）：{live.map((s) => s.name).join('、')}。
          </>
        ) : (
          <>名下没有在用的订阅。</>
        ),
        <>账本与订单不变；解封后订阅按其余条件恢复。</>,
        user.openOrderCount ? <>还有 {user.openOrderCount} 笔未完成订单，不会自动取消，请另行处理。</> : <>没有未完成订单。</>,
      ]}
      onConfirm={(reason) => api.adminBanUser({ userId: user.id, body: { reason } })}
      onDone={() => {
        message.success('已封禁')
        void qc.invalidateQueries({ queryKey: ['admin'] })
      }}
    />
  )
}

function UnbanDialog({ user, open, onClose }: { user: AdminUserDetail; open: boolean; onClose: () => void }) {
  const api = useApi()
  const qc = useQueryClient()
  const { message } = App.useApp()
  return (
    <ConfirmAction
      open={open}
      onClose={onClose}
      tone="warn"
      title={`解除封禁 ${user.email}`}
      okText="解除封禁"
      impacts={[<>账号可以重新登录。</>, <>名下订阅解除“账号封禁”阻断；其他阻断（到期、耗尽、暂停等）仍然生效。</>]}
      onConfirm={() => api.adminUnbanUser({ userId: user.id })}
      onDone={() => {
        message.success('已解除封禁')
        void qc.invalidateQueries({ queryKey: ['admin'] })
      }}
    />
  )
}

function ResetDialog({ user, open, onClose }: { user: AdminUserDetail; open: boolean; onClose: () => void }) {
  const api = useApi()
  const { message } = App.useApp()
  return (
    <ConfirmAction
      open={open}
      onClose={onClose}
      tone="info"
      title="发送重置密码邮件"
      okText="发送"
      impacts={[<>向 {user.email} 发送一封重置密码邮件。</>, <>现有密码在用户完成重置前仍然有效；管理员看不到也设置不了用户密码。</>]}
      onConfirm={() => api.adminSendPasswordReset({ userId: user.id })}
      onDone={() => message.success('邮件已发送')}
    />
  )
}
