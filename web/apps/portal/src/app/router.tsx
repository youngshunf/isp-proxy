import type { ComponentType } from 'react'
import { createBrowserRouter, Navigate } from 'react-router'
import { Shell } from './Shell'

// Data 模式纯 SPA：路由只负责拆包与布局，不用 loader 取数（数据统一走 TanStack Query，见 docs/03）。
const page = (load: () => Promise<{ default: ComponentType }>) => async () => ({ Component: (await load()).default })
const auth = (name: 'LoginPage' | 'RegisterPage' | 'VerifyEmailPage' | 'ForgotPasswordPage' | 'NotFoundPage') => async () => ({
  Component: (await import('../pages/AuthPages'))[name],
})

export const router = createBrowserRouter([
  { path: '/login', lazy: auth('LoginPage') },
  { path: '/register', lazy: auth('RegisterPage') },
  { path: '/verify-email', lazy: auth('VerifyEmailPage') },
  { path: '/forgot-password', lazy: auth('ForgotPasswordPage') },
  {
    path: '/',
    Component: Shell,
    children: [
      { index: true, element: <Navigate to="/ips" replace /> },
      { path: 'ips', lazy: page(() => import('../pages/IpsPage')) },
      { path: 'ips/:id', lazy: page(() => import('../pages/IpDetailPage')) },
      { path: 'buy', lazy: page(() => import('../pages/BuyPage')) },
      { path: 'buy/renew/:id', lazy: page(() => import('../pages/RenewPage')) },
      { path: 'orders', lazy: page(() => import('../pages/OrdersPage')) },
      { path: 'orders/:id', lazy: page(() => import('../pages/OrderDetailPage')) },
      { path: 'requests', lazy: page(() => import('../pages/RequestsPage')) },
      { path: 'account', lazy: page(() => import('../pages/AccountPage')) },
      { path: '*', lazy: auth('NotFoundPage') },
    ],
  },
])
