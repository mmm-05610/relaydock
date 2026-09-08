import { Banner, Card, Col, Radio, RadioGroup, Row, Spin, Table, Typography } from '@douyinfe/semi-ui'
import { useCallback, useEffect, useState } from 'react'
import { api, type UsageOverview } from '../api/client'
import EChart from '../components/EChart'

const { Title, Text } = Typography

function StatCard({ title, value, sub }: { title: string; value: string; sub?: string }) {
  return (
    <Card bodyStyle={{ padding: 16 }}>
      <Text type="tertiary" size="small">
        {title}
      </Text>
      <div style={{ fontSize: 24, fontWeight: 600, lineHeight: 1.4 }}>{value}</div>
      {sub && (
        <Text type="tertiary" size="small">
          {sub}
        </Text>
      )}
    </Card>
  )
}

// 分布表（Top5 + 其他）
function DistributionTable({ data, title }: { data: UsageOverview['by_model']; title: string }) {
  const total = data.reduce((acc, d) => acc + d.requests, 0)
  return (
    <Card title={title} bodyStyle={{ paddingTop: 8 }}>
      <Table
        size="small"
        pagination={false}
        dataSource={data}
        rowKey="group"
        columns={[
          {
            title: '分组',
            dataIndex: 'group',
            render: (v: string, d) => (
              <div>
                <Text>{v}</Text>
                <Text type="tertiary" size="small" style={{ marginLeft: 8 }}>
                  {total > 0 ? `${((d.requests / total) * 100).toFixed(1)}%` : ''}
                </Text>
              </div>
            ),
          },
          { title: '请求', dataIndex: 'requests', render: (v: number) => v.toLocaleString() },
          { title: 'tokens', dataIndex: 'tokens', render: (v: number) => v.toLocaleString() },
          { title: '成本', dataIndex: 'cost', render: (v: number) => `¥${v.toFixed(4)}` },
        ]}
        empty="暂无数据"
      />
    </Card>
  )
}

export default function Usage() {
  const [days, setDays] = useState(7)
  const [ov, setOv] = useState<UsageOverview | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const load = useCallback(async (d: number) => {
    setLoading(true)
    setError('')
    try {
      setOv(await api.overview(d))
    } catch (e) {
      setError(String((e as Error).message ?? e))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load(days)
  }, [days, load])

  const s = ov?.summary
  const series = ov?.series ?? []

  const trendOption = {
    tooltip: { trigger: 'axis' as const },
    legend: { data: ['请求', '错误', '成本(元)'] },
    grid: { left: 56, right: 56, top: 40, bottom: 32 },
    xAxis: { type: 'category' as const, data: series.map((p) => p.date) },
    yAxis: [
      { type: 'value' as const, name: '请求' },
      { type: 'value' as const, name: '元', splitLine: { show: false } },
    ],
    series: [
      { name: '请求', type: 'bar' as const, data: series.map((p) => p.requests) },
      { name: '错误', type: 'bar' as const, data: series.map((p) => p.errors), itemStyle: { color: '#d6555f' } },
      { name: '成本(元)', type: 'line' as const, yAxisIndex: 1, smooth: true, data: series.map((p) => p.cost.toFixed(3)) },
    ],
  }

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
        <Title heading={5}>用量分析</Title>
        <RadioGroup value={days} onChange={(e) => setDays(e.target.value as number)} type="button">
          <Radio value={1}>24 小时</Radio>
          <Radio value={3}>3 天</Radio>
          <Radio value={7}>7 天</Radio>
          <Radio value={14}>14 天</Radio>
          <Radio value={30}>30 天</Radio>
        </RadioGroup>
      </div>

      {error && <Banner type="danger" description={error} style={{ marginBottom: 16 }} />}
      {loading ? (
        <Spin style={{ display: 'block', margin: '80px auto' }} />
      ) : (
        <>
          <Row gutter={[16, 16]}>
            <Col span={6}>
              <StatCard title="请求总数" value={(s?.requests ?? 0).toLocaleString()} sub={`成功率 ${((s?.success_rate ?? 0) * 100).toFixed(1)}%`} />
            </Col>
            <Col span={6}>
              <StatCard title="总成本" value={`¥ ${(s?.cost ?? 0).toFixed(4)}`} />
            </Col>
            <Col span={6}>
              <StatCard title="总 tokens" value={(s?.tokens ?? 0).toLocaleString()} sub={`输入 ${(s?.input_tokens ?? 0).toLocaleString()} / 输出 ${(s?.output_tokens ?? 0).toLocaleString()}`} />
            </Col>
            <Col span={6}>
              <StatCard
                title="缓存"
                value={`读 ${(s?.cache_read_tokens ?? 0).toLocaleString()}`}
                sub={`写 ${(s?.cache_write_tokens ?? 0).toLocaleString()} · 命中率 ${
                  s && s.input_tokens + s.cache_read_tokens > 0
                    ? `${((s.cache_read_tokens / (s.input_tokens + s.cache_read_tokens)) * 100).toFixed(1)}%`
                    : '-'
                }`}
              />
            </Col>
          </Row>

          <Card style={{ marginTop: 16 }} bodyStyle={{ paddingTop: 8 }}>
            <EChart option={trendOption} height={300} />
          </Card>

          <Row gutter={16} style={{ marginTop: 16 }}>
            <Col span={12}>
              <DistributionTable data={ov?.by_model ?? []} title="按模型（Top5 + 其他）" />
            </Col>
            <Col span={12}>
              <DistributionTable data={ov?.by_key ?? []} title="按虚拟 Key（Top5 + 其他）" />
            </Col>
          </Row>
          <Text type="tertiary" size="small" style={{ display: 'block', marginTop: 12 }}>
            粒度自动适配：24 小时/3 天按小时聚合，其余按天聚合。成本按渠道配置单价折算。
          </Text>
        </>
      )}
    </div>
  )
}
