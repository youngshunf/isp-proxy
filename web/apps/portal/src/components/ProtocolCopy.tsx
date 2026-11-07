import { CheckOutlined, CopyOutlined } from '@ant-design/icons'
import { PROTOCOLS, connectionString, type Access, type Protocol } from '@isp/api-client'
import { useCopy } from './useCopy'

export const PROTO_LABEL: Record<Protocol, string> = { http: 'HTTP', https: 'HTTPS', socks5: 'SOCKS5' }

/** 三个协议的一键复制。复制的是完整连接串（含用户名与密码）。 */
export function ProtocolCopy({ access, dim = false, label }: { access: Access; dim?: boolean; label: string }) {
  const { copied, announce, copy } = useCopy()
  return (
    <div className={`proto-copy${dim ? ' stale' : ''}`} role="group" aria-label={`复制 ${label} 的连接`}>
      {PROTOCOLS.map((p) => (
        <button
          key={p}
          type="button"
          className={copied === p ? 'copied' : undefined}
          onClick={() => copy(p, connectionString(access, p), ` ${PROTO_LABEL[p]} 连接`)}
          title={`复制 ${PROTO_LABEL[p]} 连接串`}
          aria-label={copied === p ? `已复制 ${PROTO_LABEL[p]} 连接` : `复制 ${PROTO_LABEL[p]} 连接`}
        >
          {copied === p ? <CheckOutlined /> : <CopyOutlined />}
          {copied === p ? '已复制' : PROTO_LABEL[p]}
        </button>
      ))}
      <span className="visually-hidden" aria-live="polite">
        {announce}
      </span>
    </div>
  )
}
