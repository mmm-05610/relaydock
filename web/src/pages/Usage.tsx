import { Banner, Card, Col, Radio, RadioGroup, Row, Spin, Table, Typography } from '@douyinfe/semi-ui'
import { useCallback, useEffect, useState } from 'react'
import { api, type GroupedUsage, type TimeseriesPoint } from '../api/client'
import EChart from '../components/EChart'

const { Title, Text } = Typography

export default function Usage() {
  const [days, setDays] = useState(7)
  const [series, setSeries] = useState<TimeseriesPoint[]>([])
  const [byModel, setByModel] = useState<GroupedUsage[]>([])
  const [byKey, setByKey] = useState<GroupedUsage[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const load = useCallback(async (d: number) => {
    setLoading(true)
    setError('')
    try {
      const [ts, m, k] = await Promise.all([
        api.timeseries({ days: d }),
        api.grouped({ by: 'model', days: d }),
        api.grouped({ by: 'key', days: d }),
      ])
      setSeries(ts ?? [])
      setByModel(m ?? [])
      setByKey(k ?? [])
    } catch (e) {
      setError(String((e as Error).message ?? e))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load(days)
  }, [days, load])

  const trendOption = {
    tooltip: { trigger: 'axis' as const },
    legend: { data: ['成本(元)', '请求', '缓存读 tokens'] },
    grid: { left: 56, right: 56, top: 40, bottom: 32 },
    xAxis: { type: 'category' as const, data: series.map((p) => p.date) },
    yAxis: [
      { type: 'value' as const, name: 'tokens/请求' },
      { type: 'value' as const, name: '元', splitLine: { show: false } },
    ],
    series: [
      { name: '成本(元)', type: 'line' as const, yAxisIndex: 1, smooth: true, data: series.map((p) => p.cost.toFixed(3)) },
      { name: '请求', type: 'bar' as const, data: series.map((p) => p.requests) },
      { name: '缓存读 tokens', type: 'bar' as const, data: series.map((p) => p.tokens) },
    ],
  }

  const groupedColumns = [
    { title: '分组', dataIndex: 'group' },
    { title: '请求', dataIndex: 'requests', render: (v: number) => v.toLocaleString() },
    { title: '成本', dataIndex: 'cost', render: (v: number) => `¥${v.toFixed(4)}` },
    { title: '输入', dataIndex: 'input_tokens', render: (v: number) => v.toLocaleString() },
    { title: '输出', dataIndex: 'output_tokens', render: (v: number) => v.toLocaleString() },
    { title: '缓存读', dataIndex: 'cache_read_tokens', render: (v: number) => v.toLocaleString() },
    { title: '成功率', dataIndex: 'success_rate', render: (v: number) => `${(v * 100).toFixed(1)}%` },
    { title: '平均延迟', dataIndex: 'avg_latency_ms', render: (v: number) => `${Math.round(v)}ms` },
  ]

  const costByModel = {
    tooltip: { trigger: 'axis' as const },
    grid: { left: 140, right: 24, top: 16, bottom: 24 },
    xAxis: { type: 'value' as const },
    yAxis: { type: 'category' as const, data: byModel.slice(0, 10).map((g) => g.group) },
    series: [{ type: 'bar' as const, data: byModel.slice(0, 10).map((g) => g.cost.toFixed(3)), name: '成本(元)' }],
  }

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
        <Title heading={5}>用量分析</Title>
        <RadioGroup value={days} onChange={(e) => setDays(e.target.value as number)} type="button">
          <Radio value={7}>近 7 天</Radio>
          <Radio value={14}>近 14 天</Radio>
          <Radio value={30}>近 30 天</Radio>
        </RadioGroup>
      </div>

      {error && <Banner type="danger" description={error} style={{ marginBottom: 16 }} />}
      {loading ? (
        <Spin style={{ display: 'block', margin: '80px auto' }} />
      ) : (
        <>
          <Card bodyStyle={{ paddingTop: 8 }}>
            <EChart option={trendOption} height={280} />
          </Card>

          <Row gutter={16} style={{ marginTop: 16 }}>
            <Col span={14}>
              <Card title="按模型" bodyStyle={{ paddingTop: 8 }}>
                <Table size="small" pagination={false} dataSource={byModel} rowKey="group" columns={groupedColumns} empty="暂无数据" />
              </Card>
            </Col>
            <Col span={10}>
              <Card title="成本 TOP 模型" bodyStyle={{ paddingTop: 8 }}>
                <EChart option={costByModel} height={Math.max(200, byModel.length * 32)} />
              </Card>
              <Card title="按 Key" bodyStyle={{ paddingTop: 8, marginTop: 16 }}>
                <Table
                  size="small"
                  pagination={false}
                  dataSource={byKey}
                  rowKey="group"
                  columns={[
                    { title: 'Key', dataIndex: 'group' },
                    { title: '请求', dataIndex: 'requests', render: (v: number) => v.toLocaleString() },
                    { title: '成本', dataIndex: 'cost', render: (v: number) => `¥${v.toFixed(4)}` },
                    { title: '成功率', dataIndex: 'success_rate', render: (v: number) => `${(v * 100).toFixed(1)}%` },
                  ]}
                  empty="暂无数据"
                />
              </Card>
            </Col>
          </Row>
          <Text type="tertiary" size="small" style={{ display: 'block', marginTop: 12 }}>
            成本按渠道配置的模型单价（元/M tokens）折算，缓存读/写独立计价。
          </Text>
        </>
      )}
    </div>
  )
}
