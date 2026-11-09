// 管理后台通用表格（docs/05 F-W2）：筛选、游标分页、列配置。
// - 筛选值写在地址栏查询串里：可分享、可从概览直接链到“待核款订单”，刷新不丢。
// - 分页对应契约的 cursor + limit：保留走过的游标栈，支持上一页/下一页；筛选变化回到第一页。
// - 列配置（显示/隐藏）按表格 id 存在本机 localStorage；读写失败时退回默认列。
import { LeftOutlined, ReloadOutlined, RightOutlined, SettingOutlined } from '@ant-design/icons'
import { keepPreviousData, useQuery } from '@tanstack/react-query'
import { Button, Checkbox, DatePicker, Input, Popover, Select, Table, type TableProps } from 'antd'
import type { ColumnType } from 'antd/es/table'
import dayjs from 'dayjs'
import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { useSearchParams } from 'react-router'
import type { Page } from '@isp/api-client/admin'
import { errorText } from '../app/api'

export type FilterValue = string | string[] | undefined
export type Filters = Record<string, FilterValue>

export interface FilterDef {
  key: string
  label: string
  type: 'search' | 'select' | 'multi' | 'bool' | 'date'
  options?: { value: string; label: string }[]
  placeholder?: string
  width?: number
}

export type Column<T> = ColumnType<T> & {
  key: string
  title: string
  /** 不允许隐藏（主键、操作列）。 */
  fixedVisible?: boolean
  defaultHidden?: boolean
}

export interface DataTableProps<T> {
  id: string
  columns: Column<T>[]
  filters?: FilterDef[]
  queryKey: readonly unknown[]
  fetchPage: (args: { filters: Filters; cursor: string | undefined; limit: number }) => Promise<Page<T>>
  rowKey: (row: T) => string
  toolbar?: ReactNode
  emptyText?: ReactNode
  rowClassName?: (row: T) => string
  expandable?: TableProps<T>['expandable']
  pageSizes?: number[]
  refetchInterval?: number
  /** 表格的无障碍名称。 */
  label: string
}

const PAGE_PARAM = 'page'

function readFilters(sp: URLSearchParams, defs: FilterDef[]): Filters {
  const f: Filters = {}
  for (const d of defs) {
    const raw = sp.get(d.key)
    if (raw === null || raw === '') continue
    f[d.key] = d.type === 'multi' ? raw.split(',').filter(Boolean) : raw
  }
  return f
}

function loadHidden(id: string, cols: Column<unknown>[]): string[] {
  try {
    const raw = localStorage.getItem(`isp-admin:cols:${id}`)
    if (raw) return JSON.parse(raw) as string[]
  } catch {
    /* 存储不可用时用默认列 */
  }
  return cols.filter((c) => c.defaultHidden).map((c) => c.key)
}

export function DataTable<T extends object>(props: DataTableProps<T>) {
  const { id, columns, filters: defs = [], queryKey, fetchPage, rowKey, toolbar, emptyText, label } = props
  const pageSizes = props.pageSizes ?? [20, 50, 100]
  const [sp, setSp] = useSearchParams()
  const filters = useMemo(() => readFilters(sp, defs), [sp, defs])
  const filterSig = JSON.stringify(filters)

  const [limit, setLimit] = useState(pageSizes[0]!)
  const [stack, setStack] = useState<(string | undefined)[]>([undefined])
  const cursor = stack[stack.length - 1]
  useEffect(() => setStack([undefined]), [filterSig, limit])

  const [hidden, setHidden] = useState<string[]>(() => loadHidden(id, columns as Column<unknown>[]))
  const saveHidden = (next: string[]) => {
    setHidden(next)
    try {
      localStorage.setItem(`isp-admin:cols:${id}`, JSON.stringify(next))
    } catch {
      /* 忽略 */
    }
  }

  const q = useQuery({
    queryKey: [...queryKey, 'page', filterSig, cursor ?? '', limit],
    queryFn: () => fetchPage({ filters, cursor, limit }),
    placeholderData: keepPreviousData,
    refetchInterval: props.refetchInterval,
  })

  const setFilter = (key: string, value: FilterValue) => {
    const next = new URLSearchParams(sp)
    const v = Array.isArray(value) ? value.join(',') : value
    if (v === undefined || v === '') next.delete(key)
    else next.set(key, v)
    next.delete(PAGE_PARAM)
    setSp(next, { replace: true })
  }
  const activeCount = defs.filter((d) => filters[d.key] !== undefined).length

  const visible = columns.filter((c) => c.fixedVisible || !hidden.includes(c.key))
  const settings = (
    <div className="col-settings">
      <div className="col-settings-title">显示的列</div>
      {columns.map((c) => (
        <Checkbox
          key={c.key}
          disabled={c.fixedVisible}
          checked={c.fixedVisible || !hidden.includes(c.key)}
          onChange={(e) => saveHidden(e.target.checked ? hidden.filter((h) => h !== c.key) : [...hidden, c.key])}
        >
          {c.title}
        </Checkbox>
      ))}
      <Button size="small" type="link" onClick={() => saveHidden(columns.filter((c) => c.defaultHidden).map((c) => c.key))}>
        恢复默认
      </Button>
    </div>
  )

  return (
    <div className="dt">
      {(defs.length > 0 || toolbar) && (
        <div className="dt-bar" role="search" aria-label={`${label}筛选`}>
          <div className="dt-filters">
            {defs.map((d) => (
              <FilterControl key={d.key} def={d} value={filters[d.key]} onChange={(v) => setFilter(d.key, v)} />
            ))}
            {activeCount > 0 && (
              <Button
                type="link"
                size="small"
                onClick={() => {
                  const next = new URLSearchParams(sp)
                  for (const d of defs) next.delete(d.key)
                  setSp(next, { replace: true })
                }}
              >
                清除筛选（{activeCount}）
              </Button>
            )}
          </div>
          <div className="dt-tools">
            {toolbar}
            <Button icon={<ReloadOutlined />} aria-label="刷新" title="刷新" onClick={() => void q.refetch()} loading={q.isFetching && !q.isPending} />
            <Popover content={settings} trigger="click" placement="bottomRight">
              <Button icon={<SettingOutlined />} aria-label="列设置" title="列设置" />
            </Popover>
          </div>
        </div>
      )}
      {q.isError && (
        <div className="dt-error" role="alert">
          {errorText(q.error)}
          <Button size="small" onClick={() => void q.refetch()}>
            重试
          </Button>
        </div>
      )}
      <Table<T>
        aria-label={label}
        size="small"
        rowKey={rowKey}
        columns={visible}
        dataSource={q.data?.items ?? []}
        loading={q.isPending || (q.isFetching && q.isPlaceholderData)}
        pagination={false}
        scroll={{ x: 'max-content' }}
        locale={{ emptyText: q.isError ? '加载失败' : (emptyText ?? (activeCount ? '没有符合筛选条件的记录' : '暂无记录')) }}
        rowClassName={props.rowClassName}
        expandable={props.expandable}
      />
      <div className="dt-pager" aria-label="分页">
        <span className="muted">
          第 {stack.length} 页 · 本页 {q.data?.items.length ?? 0} 条
        </span>
        <Select
          size="small"
          value={limit}
          onChange={setLimit}
          options={pageSizes.map((n) => ({ value: n, label: `每页 ${n}` }))}
          aria-label="每页条数"
          style={{ width: 96 }}
        />
        <Button size="small" icon={<LeftOutlined />} disabled={stack.length <= 1} onClick={() => setStack((s) => s.slice(0, -1))}>
          上一页
        </Button>
        <Button
          size="small"
          disabled={!q.data?.nextCursor || q.isPlaceholderData}
          onClick={() => setStack((s) => [...s, q.data!.nextCursor!])}
        >
          下一页 <RightOutlined />
        </Button>
      </div>
    </div>
  )
}

function FilterControl({ def, value, onChange }: { def: FilterDef; value: FilterValue; onChange: (v: FilterValue) => void }) {
  const [text, setText] = useState(typeof value === 'string' ? value : '')
  useEffect(() => setText(typeof value === 'string' ? value : ''), [value])
  switch (def.type) {
    case 'search':
      return (
        <Input.Search
          aria-label={def.label}
          placeholder={def.placeholder ?? def.label}
          allowClear
          value={text}
          onChange={(e) => {
            setText(e.target.value)
            if (e.target.value === '') onChange(undefined)
          }}
          onSearch={(v) => onChange(v.trim() || undefined)}
          style={{ width: def.width ?? 260 }}
        />
      )
    case 'select':
    case 'bool':
      return (
        <Select
          aria-label={def.label}
          placeholder={def.label}
          allowClear
          value={typeof value === 'string' ? value : undefined}
          onChange={(v) => onChange(v ?? undefined)}
          options={def.type === 'bool' ? (def.options ?? [{ value: 'true', label: '是' }, { value: 'false', label: '否' }]) : def.options}
          showSearch={def.type === 'select' && (def.options?.length ?? 0) > 12}
          optionFilterProp="label"
          style={{ width: def.width ?? 150 }}
          popupMatchSelectWidth={false}
        />
      )
    case 'multi':
      return (
        <Select
          mode="multiple"
          aria-label={def.label}
          placeholder={def.label}
          allowClear
          maxTagCount="responsive"
          value={Array.isArray(value) ? value : []}
          onChange={(v: string[]) => onChange(v.length ? v : undefined)}
          options={def.options}
          style={{ width: def.width ?? 200 }}
          popupMatchSelectWidth={false}
        />
      )
    case 'date':
      return (
        <DatePicker
          aria-label={def.label}
          placeholder={def.label}
          value={typeof value === 'string' ? dayjs(value) : null}
          onChange={(d) => onChange(d ? d.startOf('day').toISOString() : undefined)}
          style={{ width: def.width ?? 150 }}
        />
      )
  }
}

/** 把筛选值转成接口查询参数（多选保持数组，布尔转 boolean）。 */
export function str(f: Filters, k: string): string | undefined {
  const v = f[k]
  return typeof v === 'string' ? v : undefined
}
export function arr<T extends string>(f: Filters, k: string): T[] | undefined {
  const v = f[k]
  return Array.isArray(v) ? (v as T[]) : typeof v === 'string' ? [v as T] : undefined
}
export function bool(f: Filters, k: string): boolean | undefined {
  const v = f[k]
  return v === 'true' ? true : v === 'false' ? false : undefined
}
