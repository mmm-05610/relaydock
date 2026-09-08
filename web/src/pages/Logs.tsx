import { Banner, Button, Card, Input, Select, SideSheet, Spin, Table, Tabs, Tag, Typography } from '@douyinfe/semi-ui'
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
  const [requestId, setRequestId] = useState('')
  const [days, setDays] = useState(7)
  const [failedOnly, setFailedOnly] = useState(false)
  const [bodyTarget, setBodyTarget] = useState<UsageLog | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const pageSize = 20

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const data = await api.logs({
        limit: pageSize,
        offset: page * pageSize,
        model,
        status: status ? Number(status) : undefined,
        request_id: requestId || undefined,
        days: days || undefined,
        failed_only: failedOnly || undefined,
      })
      setLogs(data ?? [])
      // 服务端无 count：取满一页假定还有下一页
      setTotal(data.length === pageSize ? (page + 1) * pageSize + 1 : (page + 1) * pageSize)
    } catch (e) {
      setError(String((e as Error).message ?? e))
    } finally {
      setLoading(false)
    }
  }, [page, model, status, requestId, days, failedOnly])

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
          <Input placeholder="按模型名筛选" value={model} onChange={setModel} style={{ width: 180 }} showClear />
          <Input placeholder="按状态码（如 200）" value={status} onChange={setStatus} style={{ width: 150 }} showClear />
          <Input placeholder="按 request_id 精确筛选" value={requestId} onChange={setRequestId} style={{ width: 240 }} showClear />
          <Select value={String(days)} onChange={(v) => setDays(Number(v))} style={{ width: 130 }}>
            <Select.Option value="1">最近 24 小时</Select.Option>
            <Select.Option value="7">最近 7 天</Select.Option>
            <Select.Option value="30">最近 30 天</Select.Option>
            <Select.Option value="0">全部</Select.Option>
          </Select>
          <Button onClick={() => { setPage(0); load() }}>查询</Button>
          <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 13 }}>
            <input type="checkbox" checked={failedOnly} onChange={(e) => { setFailedOnly(e.target.checked); setPage(0) }} />
            仅失败（status ≥ 400）
          </label>
          <Button onClick={() => window.open(`/api/logs/export?days=${days || 30}`)}>导出 CSV</Button>
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
            {
              title: '时间',
              dataIndex: 'created_at',
              width: 165,
              render: (v: string, rec: UsageLog) => (
                <Text
                  size="small"
                  copyable={rec.request_id ? { content: rec.request_id } : undefined}
                >
                  {new Date(v).toLocaleString('zh-CN')}
                </Text>
              ),
            },
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
            {
              title: '操作',
              width: 90,
              render: (_: unknown, rec: UsageLog) =>
                rec.request_id ? (
                  <Button
                    size="small"
                    onClick={async () => {
                      setBodyTarget(rec)
                    }}
                  >
                    全文
                  </Button>
                ) : (
                  <Text type="tertiary">-</Text>
                ),
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

      <LogBodyDrawer rec={bodyTarget} onClose={() => setBodyTarget(null)} />
    </div>
  )
}

// 全文详情抽屉：请求 / 响应 双 Tab
function LogBodyDrawer({ rec, onClose }: { rec: UsageLog | null; onClose: () => void }) {
  const [data, setData] = useState<Awaited<ReturnType<typeof api.logBody>> | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!rec?.request_id) return
    setData(null)
    setError('')
    api
      .logBody(rec.request_id)
      .then(setData)
      .catch((e) => setError(String((e as Error).message ?? e)))
  }, [rec])

  if (!rec) return null
  const pretty = (s2: string) => {
    try {
      return JSON.stringify(JSON.parse(s2), null, 2)
    } catch {
      return s2
    }
  }
  return (
    <SideSheet title={<span>请求全文 · {rec.model}</span>} visible onCancel={onClose} width={860} bodyStyle={{ padding: 16 }}>
      {error && <Banner type="danger" description={error} />}
      {!data && !error && <Spin style={{ display: 'block', margin: '40px auto' }} />}
      {data && (
        <>
          <Banner
            type="info"
            closeIcon={null}
            description={
              <span>
                {data.model} · HTTP {data.status} · {new Date(data.created_at).toLocaleString('zh-CN')}
                {data.truncated && <Tag color="orange" style={{ marginLeft: 8 }}>内容超长已截断</Tag>}
              </span>
            }
            style={{ marginBottom: 12 }}
          />
          <Tabs type="line">
            <Tabs.TabPane tab="请求体" itemKey="req">
              <pre style={{ maxHeight: 520, overflow: 'auto', background: 'var(--semi-color-fill-0)', padding: 12, borderRadius: 6, fontSize: 12, whiteSpace: 'pre-wrap' }}>
                {pretty(data.request_body)}
              </pre>
            </Tabs.TabPane>
            <Tabs.TabPane tab="响应体" itemKey="resp">
              <pre style={{ maxHeight: 520, overflow: 'auto', background: 'var(--semi-color-fill-0)', padding: 12, borderRadius: 6, fontSize: 12, whiteSpace: 'pre-wrap' }}>
                {pretty(data.response_body)}
              </pre>
            </Tabs.TabPane>
          </Tabs>
        </>
      )}
    </SideSheet>
  )
}
