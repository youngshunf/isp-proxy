import type { ComponentType } from 'react'
import type { RouteObject } from 'react-router'
import { Shell } from './Shell'

// Data 模式纯 SPA：路由只负责拆包与布局，不用 loader 取数（数据统一走 TanStack Query，见 docs/03）。
const page = (load: () => Promise<{ default: ComponentType }>) => async () => ({ Component: (await load()).default })

export const routes: RouteObject[] = [
  { path: '/login', lazy: page(() => import('../pages/LoginPage')) },
  {
    path: '/',
    Component: Shell,
    children: [
      { index: true, lazy: page(() => import('../pages/OverviewPage')) },
      { path: 'subscriptions', lazy: page(() => import('../pages/SubscriptionsPage')) },
      { path: 'subscriptions/:id', lazy: page(() => import('../pages/SubscriptionDetailPage')) },
      { path: 'subscriptions/:id/exit-change', lazy: page(() => import('../pages/ExitChangePage')) },
      { path: 'upstreams', lazy: page(() => import('../pages/UpstreamsPage')) },
      { path: 'plans', lazy: page(() => import('../pages/PlansPage')) },
      { path: 'rules', lazy: page(() => import('../pages/RulesPage')) },
      { path: 'users', lazy: page(() => import('../pages/UsersPage')) },
      { path: 'users/:id', lazy: page(() => import('../pages/UserDetailPage')) },
      { path: 'orders', lazy: page(() => import('../pages/OrdersPage')) },
      { path: 'orders/:id', lazy: page(() => import('../pages/OrderDetailPage')) },
      { path: 'exit-requests', lazy: page(() => import('../pages/ExitRequestsPage')) },
      { path: 'monitoring', lazy: page(() => import('../pages/MonitoringPage')) },
      { path: 'alerts', lazy: page(() => import('../pages/AlertsPage')) },
      { path: 'audit', lazy: page(() => import('../pages/AuditPage')) },
      { path: 'account', lazy: page(() => import('../pages/AccountPage')) },
      { path: '*', lazy: page(() => import('../pages/NotFoundPage')) },
    ],
  },
]
