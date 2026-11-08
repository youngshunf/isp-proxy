import {
  AlertOutlined,
  ApiOutlined,
  AppstoreOutlined,
  AuditOutlined,
  ClusterOutlined,
  DashboardOutlined,
  FileProtectOutlined,
  LogoutOutlined,
  MenuOutlined,
  ProfileOutlined,
  SwapOutlined,
  TagsOutlined,
  TeamOutlined,
} from '@ant-design/icons'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Drawer, Spin } from 'antd'
import { Suspense, useEffect, useState, type ReactNode } from 'react'
import { Link, Navigate, NavLink, Outlet, ScrollRestoration, useLocation, useNavigate } from 'react-router'
import { BrandMark, TZ_LABEL } from '@isp/ui'
import { USE_MOCK, qk, useApi } from './api'

export function ProtoRibbon() {
  if (!USE_MOCK) return null
  return <div className="proto-ribbon">原型：内存模拟数据，所有用户、IP、订单与收款信息均为示意</div>
}

export function Brand() {
  return (
    <Link to="/" className="brand" aria-label="管理后台首页">
      <BrandMark />
      <span>
        驿卡<small>管理后台</small>
      </span>
    </Link>
  )
}

export function PageFallback() {
  return (
    <div style={{ display: 'grid', placeItems: 'center', minHeight: 320 }}>
      <Spin />
    </div>
  )
}

interface NavItem {
  to: string
  label: string
  icon: ReactNode
  badge?: number
  badgeTone?: 'warn' | 'stop'
}

function useNav(): { group: string; items: NavItem[] }[] {
  const api = useApi()
  const ov = useQuery({ queryKey: qk.overview, queryFn: () => api.adminGetOverview(), refetchInterval: 30_000 })
  const o = ov.data
  return [
    {
      group: '运营',
      items: [
        { to: '/', label: '运营概览', icon: <DashboardOutlined /> },
        { to: '/orders', label: '订单', icon: <ProfileOutlined />, badge: o?.orders.awaitingPayment, badgeTone: 'warn' },
        { to: '/subscriptions', label: '订阅', icon: <AppstoreOutlined /> },
        { to: '/exit-requests', label: '换出口申请', icon: <SwapOutlined />, badge: o?.exitRequests.pending, badgeTone: 'warn' },
        { to: '/users', label: '用户', icon: <TeamOutlined /> },
      ],
    },
    {
      group: '资源',
      items: [
        { to: '/upstreams', label: '上游库存', icon: <ApiOutlined /> },
        { to: '/plans', label: '套餐', icon: <TagsOutlined /> },
        { to: '/rules', label: '规则', icon: <FileProtectOutlined /> },
      ],
    },
    {
      group: '监控',
      items: [
        { to: '/monitoring', label: '节点与监控', icon: <ClusterOutlined />, badge: o?.nodes.offline, badgeTone: 'stop' },
        { to: '/alerts', label: '告警', icon: <AlertOutlined />, badge: o?.alerts.open, badgeTone: o && o.alerts.critical > 0 ? 'stop' : 'warn' },
        { to: '/audit', label: '审计', icon: <AuditOutlined /> },
      ],
    },
  ]
}

function NavList({ onNavigate }: { onNavigate?: () => void }) {
  const groups = useNav()
  return (
    <nav className="sidenav" aria-label="主导航">
      {groups.map((g) => (
        <div key={g.group} className="sidenav-group">
          <div className="sidenav-title">{g.group}</div>
          {g.items.map((n) => (
            <NavLink key={n.to} to={n.to} end={n.to === '/'} onClick={onNavigate} className={({ isActive }) => (isActive ? 'active' : undefined)}>
              {n.icon}
              <span className="sidenav-label">{n.label}</span>
              {n.badge ? (
                <span className={`nav-badge tone-${n.badgeTone ?? 'warn'}`} aria-label={`${n.badge} 项待处理`}>
                  {n.badge}
                </span>
              ) : null}
            </NavLink>
          ))}
        </div>
      ))}
    </nav>
  )
}

/** 登录后的外框：左侧导航 + 内容区。会话失效时回登录页并带回原路径。 */
export function Shell() {
  const api = useApi()
  const me = useQuery({ queryKey: qk.me, queryFn: () => api.adminMe(), staleTime: 60_000, retry: false })
  const location = useLocation()
  const navigate = useNavigate()
  const qc = useQueryClient()
  const [drawer, setDrawer] = useState(false)

  useEffect(() => setDrawer(false), [location.pathname])

  if (me.isPending) {
    return (
      <>
        <ProtoRibbon />
        <PageFallback />
      </>
    )
  }
  if (!me.data) {
    const from = location.pathname + location.search
    return <Navigate to={`/login?from=${encodeURIComponent(from)}`} replace />
  }
  const admin = me.data

  const logout = async () => {
    try {
      await api.adminLogout()
    } finally {
      qc.clear()
      navigate('/login', { replace: true })
    }
  }

  const who = (
    <div className="side-foot">
      <Link to="/account" className="who">
        <b>{admin.displayName}</b>
        <span>
          {admin.email} · {admin.role === 'viewer' ? '只读' : '管理员'}
        </span>
      </Link>
      <button type="button" className="icon-btn" onClick={logout} aria-label="退出登录" title="退出登录">
        <LogoutOutlined />
      </button>
    </div>
  )

  return (
    <div className="layout">
      <ProtoRibbon />
      <div className="layout-body">
        <aside className="side">
          <Brand />
          <NavList />
          {who}
        </aside>
        <header className="mobilebar">
          <button type="button" className="icon-btn" onClick={() => setDrawer(true)} aria-label="打开导航">
            <MenuOutlined />
          </button>
          <Brand />
        </header>
        <Drawer open={drawer} onClose={() => setDrawer(false)} placement="left" size={260} title={null} closable={false} styles={{ body: { padding: 0 } }}>
          <div className="side in-drawer">
            <Brand />
            <NavList onNavigate={() => setDrawer(false)} />
            {who}
          </div>
        </Drawer>
        <main className="main" id="main">
          {admin.role === 'viewer' && <div className="role-note">只读账号：可以查看全部页面，写操作会被服务端拒绝（403）。</div>}
          <Suspense fallback={<PageFallback />}>
            <Outlet />
          </Suspense>
          <p className="foot-note">时间按 {TZ_LABEL} 显示；流量按 1 GB = 10⁹ 字节计；金额单位为人民币元。</p>
        </main>
      </div>
      <ScrollRestoration />
    </div>
  )
}
