import { Banner, Card, Descriptions, Spin, Table, Typography } from '@douyinfe/semi-ui'
import { useEffect, useState } from 'react'
import { api, type DashboardStats, type TimeseriesPoint } from '../api/client'
import EChart from '../components/EChart'

const { Title, Text } = Typography

function StatCard({ title, value, sub }: { title: string; value: string; sub?: string }) {
  return (
    <Card bodyStyle={{ padding: 16 }}>
      <Text type="tertiary" size="small">
        {title}
      </Text>
      <div style={{ fontSize: 26, fontWeight: 600, lineHeight: 1.4 }}>{value}</div>
      {sub && (
        <Text type="tertiary" size="small">
          {sub}
        </Text>
      )}
    </Card>
  )
}

export default function Dashboard() {
  const [stats, setStats] = useState<DashboardStats | null>(null)
  const [series, setSeries] = useState<TimeseriesPoint[]>([])
  const [error, setError] = useState('')

  useEffect(() => {
    Promise.all([api.dashboard(), api.timeseries({ days: 7 })])
      .then(([d, s]) => {
        setStats(d)
        setSeries(s ?? [])
      })
      .catch((e) => setError(String(e.message ?? e)))
  }, [])

  if (error) return <Banner type="danger" description={error} />
  if (!stats) return <Spin style={{ display: 'block', margin: '80px auto' }} />

  const option = {
    tooltip: { trigger: 'axis' as const },
    legend: { data: ['请求数', '成本(元)'] },
    grid: { left: 48, right: 48, top: 40, bottom: 32 },
    xAxis: { type: 'category' as const, data: series.map((p) => p.date) },
    yAxis: [
      { type: 'value' as const, name: '请求' },
      { type: 'value' as const, name: '元', splitLine: { show: false } },
    ],
    series: [
      { name: '请求数', type: 'bar' as const, data: series.map((p) => p.requests) },
      { name: '成本(元)', type: 'line' as const, yAxisIndex: 1, smooth: true, data: series.map((p) => p.cost.toFixed(3)) },
    ],
  }

  return (
    <div>
      <Title heading={5} style={{ marginBottom: 16 }}>
        概览
      </Title>
      <div className="grid-cards">
        <StatCard title="今日成本" value={`¥ ${stats.today_cost.toFixed(4)}`} sub="按模型单价折算" />
        <StatCard title="今日请求" value={String(stats.today_requests)} />
        <StatCard title="成功率" value={`${(stats.success_rate * 100).toFixed(1)}%`} sub="status < 400 占比" />
        <StatCard title="平均延迟" value={`${Math.round(stats.avg_latency_ms)} ms`} />
        <StatCard title="缓存命中" value={`${(stats.cache_hit_rate * 100).toFixed(1)}%`} sub={`缓存读 ${stats.cache_read_tokens.toLocaleString()} tokens`} />
        <StatCard title="今日 tokens" value={stats.today_tokens.toLocaleString()} />
      </div>

      <Card title="近 7 天趋势" style={{ marginTop: 16 }} bodyStyle={{ paddingTop: 8 }}>
        <EChart option={option} height={300} />
      </Card>

      <Card title="最近请求" style={{ marginTop: 16 }} bodyStyle={{ paddingTop: 8 }}>
        <Table
          size="small"
          pagination={false}
          dataSource={stats.recent_logs ?? []}
          rowKey="id"
          columns={[
            { title: '时间', dataIndex: 'created_at', width: 170, render: (v: string) => new Date(v).toLocaleString('zh-CN') },
            { title: '模型', dataIndex: 'model', width: 160 },
            { title: '状态', dataIndex: 'status', width: 80 },
            { title: '输入', dataIndex: 'input_tokens', width: 90, render: (v: number) => v.toLocaleString() },
            { title: '输出', dataIndex: 'output_tokens', width: 90, render: (v: number) => v.toLocaleString() },
            { title: '成本', dataIndex: 'cost', width: 100, render: (v: number) => `¥${v.toFixed(6)}` },
            { title: '延迟', dataIndex: 'latency_ms', width: 90, render: (v: number) => `${v}ms` },
            {
              title: '详情',
              render: (_: unknown, rec: DashboardStats['recent_logs'][number]) => (
                <Text ellipsis={{ showTooltip: true }} style={{ maxWidth: 320 }} type="tertiary" size="small">
                  {rec.error || `${rec.protocol} → ${rec.upstream_model || '-'}`}
                </Text>
              ),
            },
          ]}
        />
      </Card>

      <Card style={{ marginTop: 16 }} bodyStyle={{ padding: 16 }}>
        <Descriptions
          row
          size="small"
          data={[
            { key: '网关', value: '纯透传 · 三协议（anthropic / responses / chat_completions）' },
            { key: '调度', value: '账号池：占用比例最低优先 + 429 冷却 + 保守 failover' },
          ]}
        />
      </Card>
    </div>
  )
}
