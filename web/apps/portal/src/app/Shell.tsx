import {
  CreditCardOutlined,
  FileTextOutlined,
  LogoutOutlined,
  ShoppingOutlined,
  SwapOutlined,
  UserOutlined,
} from '@ant-design/icons'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Dropdown, Spin } from 'antd'
import { Suspense, type ReactNode } from 'react'
import { Link, Navigate, NavLink, Outlet, ScrollRestoration, useLocation, useNavigate } from 'react-router'
import { TZ_LABEL } from '../lib/format'
import { api, qk } from './api'

export function ProtoRibbon() {
  return <div className="proto-ribbon">原型：所有 IP、价格、订单与收款信息均为示意数据</div>
}

export function Brand() {
  return (
    <Link to="/ips" className="brand" aria-label="ISP 代理，返回我的 IP">
      <span className="brand-mark" aria-hidden="true" />
      ISP 代理
    </Link>
  )
}

const NAV: { to: string; label: string; tab: string; icon: ReactNode }[] = [
  { to: '/ips', label: '我的 IP', tab: '我的 IP', icon: <CreditCardOutlined /> },
  { to: '/buy', label: '购买 IP', tab: '购买', icon: <ShoppingOutlined /> },
  { to: '/orders', label: '订单', tab: '订单', icon: <FileTextOutlined /> },
  { to: '/requests', label: '申请', tab: '申请', icon: <SwapOutlined /> },
]

export function PageFallback() {
  return (
    <div style={{ display: 'grid', placeItems: 'center', minHeight: 320 }}>
      <Spin />
    </div>
  )
}

/** 登录后的外框：会话失效或邮箱未验证时转去对应页面。 */
export function Shell() {
  const session = useQuery({ queryKey: qk.session, queryFn: () => api.session(), staleTime: 60_000 })
  const location = useLocation()
  const navigate = useNavigate()
  const qc = useQueryClient()

  if (session.isPending) {
    return (
      <>
        <ProtoRibbon />
        <PageFallback />
      </>
    )
  }
  const account = session.data
  if (!account) return <Navigate to="/login" replace state={{ from: location.pathname }} />
  if (!account.verified) return <Navigate to="/verify-email" replace />

  const logout = async () => {
    await api.logout()
    qc.clear()
    navigate('/login', { replace: true })
  }

  return (
    <>
      <ProtoRibbon />
      <header className="topbar">
        <div className="topbar-inner">
          <Brand />
          <nav className="mainnav" aria-label="主导航">
            {NAV.map((n) => (
              <NavLink key={n.to} to={n.to} className={({ isActive }) => (isActive ? 'active' : undefined)}>
                {n.label}
              </NavLink>
            ))}
          </nav>
          <div className="topbar-account">
            <Dropdown
              trigger={['click']}
              menu={{
                items: [
                  { key: 'account', icon: <UserOutlined />, label: '账号与密码', onClick: () => navigate('/account') },
                  { type: 'divider' },
                  { key: 'logout', icon: <LogoutOutlined />, label: '退出登录', onClick: logout },
                ],
              }}
            >
              <button type="button" className="account-btn">
                <UserOutlined />
                {account.email}
              </button>
            </Dropdown>
          </div>
        </div>
      </header>
      <main className="page" id="main">
        <Suspense fallback={<PageFallback />}>
          <Outlet />
        </Suspense>
        <p className="plate-note" style={{ marginTop: 48 }}>
          时间按 {TZ_LABEL} 显示；流量按 1 GB = 10⁹ 字节计。
        </p>
      </main>
      <nav className="tabbar" aria-label="主导航">
        {[...NAV, { to: '/account', label: '账号', tab: '账号', icon: <UserOutlined /> }].map((n) => (
          <NavLink key={n.to} to={n.to} className={({ isActive }) => (isActive ? 'active' : undefined)}>
            {n.icon}
            <span>{n.tab}</span>
          </NavLink>
        ))}
      </nav>
      <ScrollRestoration />
    </>
  )
}
