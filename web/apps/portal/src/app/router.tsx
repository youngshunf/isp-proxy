import type { ComponentType } from 'react'
import { Navigate, type RouteObject } from 'react-router'
import { Shell } from './Shell'

// Data 模式纯 SPA：路由只负责拆包与布局，不用 loader 取数（数据统一走 TanStack Query，见 docs/03）。
const page = (load: () => Promise<{ default: ComponentType }>) => async () => ({ Component: (await load()).default })
type AuthPage = 'LoginPage' | 'RegisterPage' | 'VerifyEmailPage' | 'ForgotPasswordPage' | 'ResetPasswordPage' | 'NotFoundPage'
const auth = (name: AuthPage) => async () => ({
  Component: (await import('../pages/AuthPages'))[name],
})

export const routes: RouteObject[] = [
  { path: '/login', lazy: auth('LoginPage') },
  { path: '/register', lazy: auth('RegisterPage') },
  { path: '/verify-email', lazy: auth('VerifyEmailPage') },
  { path: '/forgot-password', lazy: auth('ForgotPasswordPage') },
  // 邮件里的重设链接（?email=&code=）或手动输入验证码都落到这里。
  { path: '/reset-password', lazy: auth('ResetPasswordPage') },
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
]
