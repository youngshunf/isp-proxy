import type { ReactNode } from 'react'

/** 右上角 45° 切角的白色容器。页面上的一级分区都用它；内部不再嵌套卡片。 */
export function Plate({
  title,
  extra,
  children,
  small = false,
  className = '',
  as: Tag = 'section',
  labelledBy,
}: {
  title?: ReactNode
  extra?: ReactNode
  children: ReactNode
  small?: boolean
  className?: string
  as?: 'section' | 'article' | 'div'
  labelledBy?: string
}) {
  return (
    <div className={`plate-wrap ${className}`}>
      <Tag className={`plate${small ? ' sm' : ''}`} aria-labelledby={labelledBy}>
        <div className="plate-body">
          {(title || extra) && (
            <div className="plate-head">
              {typeof title === 'string' ? <h2 id={labelledBy}>{title}</h2> : title}
              {extra}
            </div>
          )}
          {children}
        </div>
      </Tag>
    </div>
  )
}
