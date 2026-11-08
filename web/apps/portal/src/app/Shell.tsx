import {
  CreditCardOutlined,
  FileTextOutlined,
  LogoutOutlined,
  ShoppingOutlined,
  SwapOutlined,
  UserOutlined,
} from '@ant-design/icons'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, Dropdown, Result, Spin } from 'antd'
import { Suspense, useEffect, type ReactNode } from 'react'
import { Link, Navigate, NavLink, Outlet, ScrollRestoration, useLocation, useNavigate } from 'react-router'
import { BrandMark } from '@isp/ui'
import { TZ_LABEL } from '../lib/format'
import { USE_MOCK, api, errorText, qk, setQueryUser } from './api'

/** 只在 mock 模式（VITE_PORTAL_API=mock）显示：真实数据下没有任何示意值。 */
export function ProtoRibbon() {
  if (!USE_MOCK) return null
  return <div className="proto-ribbon">演示模式：所有 IP、价格、订单与收款信息均为示意数据</div>
}

/** 品牌名来自后端目录（Catalog.brand），前端不写死（docs/05 §6 第 6 步）。 */
export function useBrand(): string | undefined {
  const catalog = useQuery({ queryKey: qk.catalog, queryFn: () => api.getCatalog(), staleTime: 5 * 60_000 })
  return catalog.data?.brand
}

/** 设置标签页标题：“页面 · 品牌”。 */
export function useTitle(title: string) {
  const brand = useBrand()
  useEffect(() => {
    document.title = brand ? `${title} · ${brand}` : title
  }, [title, brand])
}

export function Brand() {
  const brand = useBrand()
  return (
    <Link to="/ips" className="brand" aria-label={`${brand ?? '首页'}，返回我的 IP`}>
      <BrandMark />
      <span className="brand-name">{brand ?? '\u00a0'}</span>
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
  useTitle('我的 IP')
  // 查询键带当前用户：在子页面渲染前确定（docs/05 §6 第 2 步）。
  if (session.data) setQueryUser(session.data.email)

  if (session.isPending) {
    return (
      <>
        <ProtoRibbon />
        <PageFallback />
      </>
    )
  }
  if (session.isError) {
    return (
      <>
        <ProtoRibbon />
        <Result
          status="warning"
          title="暂时连不上服务"
          subTitle={errorText(session.error)}
          extra={<Button onClick={() => session.refetch()}>重试</Button>}
        />
      </>
    )
  }
  const account = session.data
  if (!account) return <Navigate to={`/login?next=${encodeURIComponent(location.pathname + location.search)}`} replace />
  if (!account.verified) return <Navigate to="/verify-email" replace />

  const logout = async () => {
    try {
      await api.logout()
    } finally {
      qc.clear()
      setQueryUser(null)
      navigate('/login', { replace: true })
    }
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
