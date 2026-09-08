import * as echarts from 'echarts'
import { useEffect, useRef } from 'react'

// ECharts 薄封装：初始化 / 响应式 resize / 卸载释放
export default function EChart({ option, height = 300 }: { option: echarts.EChartsOption; height?: number }) {
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const el = ref.current
    if (!el) return
    const chart = echarts.init(el)
    chart.setOption(option)
    const ro = new ResizeObserver(() => chart.resize())
    ro.observe(el)
    return () => {
      ro.disconnect()
      chart.dispose()
    }
  }, [option])

  return <div ref={ref} style={{ height, width: '100%' }} />
}
