// ECharts 用量柱图（按需引入，单独拆包；docs/05 F-W5）。只画一个序列：每个时段的字节数。
// 无样本的时段画成灰色斜纹底带（不是 0），有样本但为 0 的时段在基线上画短横标记。
import { BarChart, CustomChart } from 'echarts/charts'
import { GridComponent, MarkAreaComponent, TooltipComponent } from 'echarts/components'
import * as echarts from 'echarts/core'
import { SVGRenderer } from 'echarts/renderers'
import { useEffect, useRef } from 'react'
import type { UsageSeries } from '@isp/api-client/admin'
import { FONT_SANS, fmtBytes, fmtDateTime } from '@isp/ui'
import { bucketKind, bucketLabel } from '../lib/usageView'

echarts.use([BarChart, CustomChart, GridComponent, TooltipComponent, MarkAreaComponent, SVGRenderer])

const INK_3 = '#5c6b66'
const LINE_2 = '#e4e9e7'
const TEAL = '#00505c'
const GAP_FILL = '#e3e8e6'

function fmtTick(ms: number, bucketSeconds: number): string {
  const d = new Date(ms + 8 * 3600_000)
  const p = (n: number) => String(n).padStart(2, '0')
  if (bucketSeconds >= 86_400) return `${d.getUTCMonth() + 1}/${d.getUTCDate()}`
  if (bucketSeconds >= 3600) return `${d.getUTCMonth() + 1}/${d.getUTCDate()} ${p(d.getUTCHours())}时`
  return `${p(d.getUTCHours())}:${p(d.getUTCMinutes())}`
}

export default function UsageChart({ series, height = 240 }: { series: UsageSeries; height?: number }) {
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const el = ref.current
    if (!el) return
    const chart = echarts.init(el, undefined, { renderer: 'svg' })
    const ms = series.bucketSeconds * 1000
    const pts = series.points.map((p) => ({ t: Date.parse(p.at), p, k: bucketKind(p) }))
    // 连续的无样本时段合并成一段底带
    const gaps: [number, number][] = []
    for (const x of pts) {
      if (x.k !== 'gap') continue
      const last = gaps.at(-1)
      if (last && last[1] === x.t) last[1] = x.t + ms
      else gaps.push([x.t, x.t + ms])
    }
    chart.setOption({
      animation: false,
      textStyle: { fontFamily: FONT_SANS },
      grid: { left: 8, right: 12, top: 12, bottom: 4, containLabel: true },
      xAxis: {
        type: 'time',
        min: Date.parse(series.from),
        max: Date.parse(series.to),
        axisLine: { lineStyle: { color: '#c3cdc9' } },
        axisTick: { show: false },
        splitLine: { show: false },
        axisLabel: { color: INK_3, fontSize: 11, hideOverlap: true, formatter: (v: number) => fmtTick(v, series.bucketSeconds) },
      },
      yAxis: {
        type: 'value',
        min: 0,
        splitNumber: 3,
        axisLabel: { color: INK_3, fontSize: 11, formatter: (v: number) => (v === 0 ? '0' : fmtBytes(v)) },
        splitLine: { lineStyle: { color: LINE_2 } },
      },
      tooltip: {
        trigger: 'axis',
        axisPointer: { type: 'line', lineStyle: { color: '#8a9893', type: 'dashed' } },
        borderColor: '#d2dad7',
        borderRadius: 2,
        padding: [8, 10],
        textStyle: { color: '#14231f', fontSize: 12 },
        formatter: (items: { dataIndex: number }[]) => {
          const x = pts[items[0]?.dataIndex ?? -1]
          if (!x) return ''
          const head = `<div style="color:${INK_3}">${fmtDateTime(x.p.at)} 起 ${bucketLabel(series.bucketSeconds)}</div>`
          if (x.k === 'gap') return `${head}<b>无样本</b><div style="color:${INK_3}">没有上报，不是 0</div>`
          const v = x.k === 'zero' ? '0 B（有样本）' : fmtBytes(x.p.bytes!)
          return `${head}<b>${v}</b><div style="color:${INK_3}">峰值连接 ${x.p.maxConns ?? '—'} · 样本 ${x.p.samples}</div>`
        },
      },
      series: [
        {
          type: 'bar',
          name: '流量',
          barMaxWidth: 14,
          barCategoryGap: '20%',
          itemStyle: { color: TEAL, borderRadius: [2, 2, 0, 0] },
          // 无样本的时段在数据里用 '-'，ECharts 不画柱、tooltip 仍能命中
          data: pts.map((x) => [x.t + ms / 2, x.k === 'gap' ? '-' : x.p.bytes]),
          markArea: {
            silent: true,
            itemStyle: { color: GAP_FILL, opacity: 0.7 },
            label: { show: gaps.length <= 6, position: 'insideTop', color: INK_3, fontSize: 11, formatter: '无样本' },
            data: gaps.map(([a, b]) => [{ xAxis: a }, { xAxis: b }]),
          },
        },
        {
          // 零值标记：基线上 2px 高的短横，区别于“没有柱”的无样本
          type: 'custom',
          name: '零值',
          silent: true,
          tooltip: { show: false },
          data: pts.filter((x) => x.k === 'zero').map((x) => [x.t + ms / 2, 0]),
          renderItem: (_params: unknown, api: { value: (i: number) => number; coord: (v: number[]) => number[]; size: (v: number[]) => number[] }) => {
            const [cx, cy] = api.coord([api.value(0), 0]) as [number, number]
            const w = Math.max(4, Math.min(14, (api.size([ms, 0])[0] ?? 8) * 0.8))
            return { type: 'rect', shape: { x: cx - w / 2, y: cy - 3, width: w, height: 3 }, style: { fill: '#8a9893' } }
          },
        },
      ],
    })
    const ro = new ResizeObserver(() => chart.resize())
    ro.observe(el)
    return () => {
      ro.disconnect()
      chart.dispose()
    }
  }, [series])

  return <div ref={ref} className="usage-canvas" style={{ height }} role="img" aria-label="每个时段的流量柱图；灰色底带为无样本，基线短横为零值" />
}
