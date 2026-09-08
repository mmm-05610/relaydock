import { Banner, Button, Card, Input, Table, Tag, Typography } from '@douyinfe/semi-ui'
import { useCallback, useEffect, useState } from 'react'
import { api, type UsageLog } from '../api/client'

const { Title, Text } = Typography

const statusColor = (s: number) =>
  s < 300 ? 'green' : s < 400 ? 'blue' : s < 500 ? 'orange' : 'red'

export default function Logs() {
  const [logs, setLogs] = useState<UsageLog[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(0)
  const [model, setModel] = useState('')
  const [status, setStatus] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const pageSize = 20

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const data = await api.logs({ limit: pageSize, offset: page * pageSize, model, status: status ? Number(status) : undefined })
      setLogs(data ?? [])
      // 服务端无 count：取满一页假定还有下一页
      setTotal(data.length === pageSize ? (page + 1) * pageSize + 1 : (page + 1) * pageSize)
    } catch (e) {
      setError(String((e as Error).message ?? e))
    } finally {
      setLoading(false)
    }
  }, [page, model, status])

  useEffect(() => {
    load()
  }, [load])

  return (
    <div>
      <Title heading={5} style={{ marginBottom: 16 }}>
        请求日志
      </Title>

      <Card bodyStyle={{ padding: 12 }}>
        <div style={{ display: 'flex', gap: 12, alignItems: 'center', flexWrap: 'wrap' }}>
          <Input placeholder="按模型名筛选" value={model} onChange={setModel} style={{ width: 220 }} showClear />
          <Input placeholder="按状态码筛选（如 200）" value={status} onChange={setStatus} style={{ width: 220 }} showClear />
          <Button onClick={() => { setPage(0); load() }}>查询</Button>
          <Text type="tertiary" size="small">
            每页 {pageSize} 条，按时间倒序
          </Text>
        </div>
      </Card>

      {error && <Banner type="danger" description={error} style={{ marginTop: 12 }} />}

      <Card bodyStyle={{ paddingTop: 8, marginTop: 12 }}>
        <Table
          size="small"
          loading={loading}
          dataSource={logs}
          rowKey="id"
          expandedRowRender={(rec?: UsageLog) =>
            rec ? (
            <div style={{ fontSize: 12, lineHeight: 1.9 }}>
              <div>
                <Text type="tertiary">request_id：</Text>
                <Text code>{rec.request_id || '-'}</Text>
              </div>
              <div>
                <Text type="tertiary">链路：</Text>
                {rec.model} → {rec.upstream_model || '-'}（{rec.protocol}）· 渠道 {rec.channel_id || '-'} · 账号 {rec.account_id || '-'} · attempts {rec.attempts}
                {rec.unmetered && <Tag color="orange" style={{ marginLeft: 8 }}>未取得 usage，估算入账</Tag>}
              </div>
              {rec.error && (
                <div>
                  <Text type="tertiary">错误：</Text>
                  <Text type="danger">{rec.error}</Text>
                </div>
              )}
            </div>
            ) : null
          }
          columns={[
            { title: '时间', dataIndex: 'created_at', width: 165, render: (v: string) => new Date(v).toLocaleString('zh-CN') },
            { title: '模型', dataIndex: 'model', width: 150 },
            {
              title: '状态',
              dataIndex: 'status',
              width: 80,
              render: (v: number) => <Tag color={statusColor(v)}>{v}</Tag>,
            },
            { title: '输入', dataIndex: 'input_tokens', width: 90, render: (v: number) => v.toLocaleString() },
            { title: '输出', dataIndex: 'output_tokens', width: 90, render: (v: number) => v.toLocaleString() },
            { title: '缓存读', dataIndex: 'cache_read_tokens', width: 100, render: (v: number) => v.toLocaleString() },
            { title: '成本', dataIndex: 'cost', width: 100, render: (v: number) => `¥${v.toFixed(6)}` },
            { title: '延迟', dataIndex: 'latency_ms', width: 85, render: (v: number) => `${v}ms` },
            {
              title: '尝试',
              dataIndex: 'attempts',
              width: 70,
              render: (v: number) => (v > 1 ? <Tag color="orange">{v} 次</Tag> : <Text type="tertiary">1</Text>),
            },
            {
              title: '错误',
              dataIndex: 'error',
              ellipsis: true,
              render: (v: string) => (v ? <Text type="danger" ellipsis={{ showTooltip: true }} style={{ maxWidth: 240 }}>{v}</Text> : <Text type="tertiary">-</Text>),
            },
          ]}
          pagination={{
            currentPage: page + 1,
            pageSize,
            total: total,
            onPageChange: (p: number) => setPage(p - 1),
          }}
          empty="暂无日志（发一个真实请求后这里会出现记录）"
        />
      </Card>
    </div>
  )
}
