// 管理端 API：类型（由 OpenAPI 生成类型推导）、真实客户端与路由表。内存 mock 在 '@isp/api-client/admin/mock'。
export * from './types'
export { ADMIN_ROUTES, type AdminPath, type HttpMethod } from './routes'
export { createAdminApi, fillPath } from './client'
export { ApiError, STATUS_OF, type ApiErrorAction, type ApiErrorCode } from '../index'
export { NetworkError, createServerClock, toQueryString, type HttpOptions } from '../http'
