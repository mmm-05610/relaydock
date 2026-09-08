import {
  Banner,
  Button,
  Card,
  SideSheet,
  Form,
  Input,
  TextArea,
  Modal,
  Popconfirm,
  Select,
  Table,
  Tabs,
  Tag,
  Typography,
} from '@douyinfe/semi-ui'
import { Toast } from '@douyinfe/semi-ui'
import { useCallback, useEffect, useRef, useState } from 'react'
import { api, type Channel, type ChannelModel, type UpstreamAccount } from '../api/client'

const { Title, Text } = Typography

export default function Channels() {
  const [channels, setChannels] = useState<Channel[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [editing, setEditing] = useState<Channel | null>(null)
  const [createVisible, setCreateVisible] = useState(false)
  const [keyTarget, setKeyTarget] = useState<Channel | null>(null)
  const [manageProvider, setManageProvider] = useState('') // 打开详情 Drawer 的渠道

  const load = useCallback(async () => {
    setLoading(true)
    try {
      setChannels(await api.channels())
      setError('')
    } catch (e) {
      setError(String((e as Error).message ?? e))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
        <Title heading={5}>渠道与账号</Title>
        <Button theme="solid" type="primary" onClick={() => setCreateVisible(true)}>
          新建渠道
        </Button>
      </div>

      {error && <Banner type="danger" description={error} style={{ marginBottom: 12 }} />}

      <Card bodyStyle={{ paddingTop: 8 }}>
        <Table
          size="small"
          loading={loading}
          dataSource={channels}
          rowKey="provider"
          columns={[
            {
              title: '渠道',
              dataIndex: 'name',
              width: 160,
              render: (v: string, ch: Channel) => (
                <div>
                  <Text strong>{v}</Text>
                  <div>
                    <Text type="tertiary" size="small">
                      {ch.provider}
                    </Text>
                  </div>
                </div>
              ),
            },
            {
              title: '凭据',
              dataIndex: 'key_configured',
              width: 120,
              render: (v: boolean, ch: Channel) =>
                v ? <Tag color="green">{ch.key_prefix || '已配置'}</Tag> : <Tag color="red">未配置</Tag>,
            },
            { title: '认证模式', dataIndex: 'auth_mode', width: 110, render: (v: string) => v || 'bearer' },
            { title: '模型数', width: 80, render: (_: unknown, ch: Channel) => ch.models?.length ?? 0 },
            {
              title: '状态',
              dataIndex: 'enabled',
              width: 90,
              render: (v: boolean) => (v ? <Tag color="green">启用</Tag> : <Tag color="grey">停用</Tag>),
            },
            {
              title: '操作',
              width: 330,
              render: (_: unknown, ch: Channel) => (
                <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                  <Button size="small" type="primary" theme="light" onClick={() => setManageProvider(ch.provider)}>
                    管理
                  </Button>
                  <Button
                    size="small"
                    onClick={async () => {
                      const r = await api.testChannel(ch.provider)
                      r.ok ? Toast.success(`测试通过（${r.latency_ms}ms）`) : Toast.error(`失败：${r.error ?? r.status}`)
                    }}
                  >
                    测试
                  </Button>
                  <Button
                    size="small"
                    onClick={async () => {
                      const r = await api.channelBalance(ch.provider)
                      Toast.info(JSON.stringify(r))
                    }}
                  >
                    余额
                  </Button>
                  <Button size="small" onClick={() => setKeyTarget(ch)}>
                    凭据
                  </Button>
                  <Button
                    size="small"
                    onClick={() => {
                      setEditing(ch)
                      setCreateVisible(true)
                    }}
                  >
                    编辑
                  </Button>
                  <Popconfirm
                    title={`删除渠道 ${ch.name} 会级联删除其模型与账号，确定？`}
                    onConfirm={async () => {
                      await api.deleteChannel(ch.provider)
                      load()
                    }}
                  >
                    <Button size="small" type="danger">
                      删除
                    </Button>
                  </Popconfirm>
                </div>
              ),
            },
          ]}
          empty="还没有渠道"
        />
      </Card>

      {createVisible && (
        <ChannelFormModal
          existing={editing}
          onClose={() => {
            setCreateVisible(false)
            setEditing(null)
          }}
          onSubmit={async (values) => {
            if (editing) await api.updateChannel(editing.provider, values)
            else await api.createChannel(values)
            setCreateVisible(false)
            setEditing(null)
            load()
          }}
        />
      )}
      {keyTarget && (
        <SetKeyModal
          channel={keyTarget}
          onClose={() => setKeyTarget(null)}
          onSubmit={async (key) => {
            await api.setChannelKey(keyTarget.provider, key)
            setKeyTarget(null)
            load()
          }}
        />
      )}
      {manageProvider && (
        <ChannelDrawer
          provider={manageProvider}
          channels={channels}
          onChannelsChanged={load}
          onClose={() => setManageProvider('')}
        />
      )}
    </div>
  )
}

// ---- 渠道详情 Drawer：模型路由 / 上游账号池 两个 Tab ----

function ChannelDrawer({
  provider,
  channels,
  onChannelsChanged,
  onClose,
}: {
  provider: string
  channels: Channel[]
  onChannelsChanged: () => void
  onClose: () => void
}) {
  const channel = channels.find((c) => c.provider === provider)
  if (!channel) return null
  return (
    <SideSheet title={<span>渠道：{channel.name}（{channel.provider}）</span>} visible onCancel={onClose} width={1040} bodyStyle={{ padding: 16 }}>
      <Tabs type="line">
        <Tabs.TabPane tab="上游账号池" itemKey="accounts">
          <AccountsPanel channel={channel} onChanged={onChannelsChanged} />
        </Tabs.TabPane>
        <Tabs.TabPane tab="模型路由" itemKey="models">
          <ModelsPanel channel={channel} onChanged={onChannelsChanged} />
        </Tabs.TabPane>
      </Tabs>
    </SideSheet>
  )
}

// ---- 上游账号池面板 ----

function AccountsPanel({ channel, onChanged }: { channel: Channel; onChanged: () => void }) {
  const [view, setView] = useState<Awaited<ReturnType<typeof api.accounts>> | null>(null)
  const [stateFilter, setStateFilter] = useState('all')
  const [selected, setSelected] = useState<number[]>([])
  const [importVisible, setImportVisible] = useState(false)
  const [createVisible, setCreateVisible] = useState(false)
  const [editingAccount, setEditingAccount] = useState<UpstreamAccount | null>(null)
  const timer = useRef<ReturnType<typeof setInterval> | undefined>(undefined)

  const load = useCallback(async () => {
    try {
      setView(await api.accounts(channel.provider, true))
    } catch {
      /* 渠道刚删掉等场景：静默 */
    }
  }, [channel.provider])

  useEffect(() => {
    load()
    timer.current = setInterval(load, 5000) // inflight/冷却状态轻量轮询
    return () => clearInterval(timer.current)
  }, [load])

  const items = (view?.items ?? []).filter((a) => stateFilter === 'all' || a.state === stateFilter)
  const summary = view?.summary

  const batch = async (action: 'enable' | 'disable' | 'delete' | 'recover') => {
    const r = await api.batchAccounts(channel.provider, action, selected)
    Toast.success(`已处理 ${r.affected.length} 个账号`)
    setSelected([])
    load()
    if (action !== 'recover') onChanged()
  }

  return (
    <div>
      {/* 池级汇总条（gpt-load 同款健康桶计数） */}
      <div style={{ display: 'flex', gap: 8, alignItems: 'center', marginBottom: 12, flexWrap: 'wrap' }}>
        <Tag color="grey" size="large">总数 {summary?.total ?? 0}</Tag>
        <Tag color="green" size="large">健康 {summary?.healthy ?? 0}</Tag>
        <Tag color="orange" size="large">冷却中 {summary?.cooling ?? 0}</Tag>
        <Tag color="light-blue" size="large">禁用 {summary?.disabled ?? 0}</Tag>
        <div style={{ flex: 1 }} />
        <Select value={stateFilter} onChange={(v) => setStateFilter(v as string)} style={{ width: 140 }}>
          <Select.Option value="all">全部状态</Select.Option>
          <Select.Option value="healthy">健康</Select.Option>
          <Select.Option value="cooling">冷却中</Select.Option>
          <Select.Option value="disabled">禁用</Select.Option>
        </Select>
        <Button onClick={() => setImportVisible(true)}>批量导入</Button>
        <Button type="primary" theme="solid" onClick={() => setCreateVisible(true)}>
          添加账号
        </Button>
      </div>

      {/* 批量操作条 */}
      {selected.length > 0 && (
        <div style={{ display: 'flex', gap: 8, marginBottom: 12, padding: '8px 12px', background: 'var(--semi-color-fill-0)', borderRadius: 6 }}>
          <Text size="small">已选 {selected.length} 个：</Text>
          <Button size="small" onClick={() => batch('enable')}>批量启用</Button>
          <Button size="small" onClick={() => batch('disable')}>批量禁用</Button>
          <Button size="small" onClick={() => batch('recover')}>批量恢复</Button>
          <Popconfirm title={`删除选中的 ${selected.length} 个账号？`} onConfirm={() => batch('delete')}>
            <Button size="small" type="danger">批量删除</Button>
          </Popconfirm>
          <Button size="small" type="tertiary" onClick={() => setSelected([])}>取消选择</Button>
        </div>
      )}

      <Table
        size="small"
        rowSelection={{
          selectedRowKeys: selected.map(String),
          onChange: (keys) => setSelected((keys ?? []).map(Number)),
          onSelectAll: (checked) => setSelected(checked ? items.map((a) => a.id) : []),
        }}
        rowKey="id"
        dataSource={items}
        pagination={false}
        columns={[
          { title: '账号', dataIndex: 'name', width: 120 },
          {
            title: '状态',
            dataIndex: 'state',
            width: 100,
            render: (v: string) =>
              v === 'healthy' ? (
                <Tag color="green">健康</Tag>
              ) : v === 'cooling' ? (
                <Tag color="orange">冷却中</Tag>
              ) : (
                <Tag color="grey">禁用</Tag>
              ),
          },
          {
            title: '并发',
            dataIndex: 'inflight',
            width: 80,
            render: (v: number, a: UpstreamAccount) => `${v}/${a.max_concurrency || '∞'}`,
          },
          {
            title: '冷却至',
            dataIndex: 'cooling_until',
            width: 100,
            render: (v: string | null) => (v ? new Date(v).toLocaleTimeString('zh-CN') : '-'),
          },
          {
            title: '7天请求 / 成本',
            width: 130,
            render: (_: unknown, a: UpstreamAccount) =>
              a.usage ? `${a.usage.requests} / ¥${a.usage.cost.toFixed(3)}` : <Text type="tertiary">-</Text>,
          },
          {
            title: '成功 / 失败',
            width: 120,
            render: (_: unknown, a: UpstreamAccount) => (
              <Text size="small">
                {a.success_count} / {a.failure_count}
                {a.consecutive_failures > 0 && <Tag color="red" size="small" style={{ marginLeft: 6 }}>连败 {a.consecutive_failures}</Tag>}
              </Text>
            ),
          },
          {
            title: '最后错误',
            dataIndex: 'last_error',
            ellipsis: true,
            render: (v: string, a: UpstreamAccount) =>
              v ? (
                <Text type="danger" size="small" ellipsis={{ showTooltip: { opts: { content: `HTTP ${a.last_status_code} ${v}` } } }} style={{ maxWidth: 180 }}>
                  {a.last_error || `HTTP ${a.last_status_code}`}
                </Text>
              ) : (
                <Text type="tertiary">-</Text>
              ),
          },
          { title: '最后使用', dataIndex: 'last_used_at', width: 100, render: (v: string | null) => (v ? new Date(v).toLocaleTimeString('zh-CN') : '-') },
          {
            title: '操作',
            width: 190,
            render: (_: unknown, a: UpstreamAccount) => (
              <div style={{ display: 'flex', gap: 6 }}>
                <Button
                  size="small"
                  onClick={async () => {
                    const r = await api.testAccount(channel.provider, a.id)
                    r.ok ? Toast.success(`测试通过（${r.latency_ms}ms）`) : Toast.error(`失败：${r.error ?? r.status}`)
                  }}
                >
                  测试
                </Button>
                {a.state === 'cooling' && (
                  <Button
                    size="small"
                    type="warning"
                    onClick={async () => {
                      try {
                        await api.recoverAccount(channel.provider, a.id, a.cooling_until)
                        Toast.success('已恢复')
                        load()
                      } catch (e) {
                        Toast.error('状态已变化（可能刚被重新限流），请重新测试后再恢复')
                        load()
                      }
                    }}
                  >
                    恢复
                  </Button>
                )}
                <Button
                  size="small"
                  onClick={() => {
                    setEditingAccount(a)
                  }}
                >
                  编辑
                </Button>
                <Popconfirm
                  title={`删除账号 ${a.name}？在途请求不受影响。`}
                  onConfirm={async () => {
                    await api.deleteAccount(channel.provider, a.id)
                    load()
                    onChanged()
                  }}
                >
                  <Button size="small" type="danger">
                    删
                  </Button>
                </Popconfirm>
              </div>
            ),
          },
        ]}
        empty={
          <Text type="tertiary" size="small">
            未配置显式账号 —— 当前走「渠道凭据」单 key 路径，行为不变；点右上「添加账号」开始用账号池
          </Text>
        }
      />

      {importVisible && (
        <ImportAccountsModal
          provider={channel.provider}
          existingCount={summary?.total ?? 0}
          onClose={() => setImportVisible(false)}
          onDone={() => {
            setImportVisible(false)
            load()
            onChanged()
          }}
        />
      )}
      {createVisible && (
        <AccountFormModal
          provider={channel.provider}
          existing={null}
          onClose={() => setCreateVisible(false)}
          onSubmit={async (values) => {
            await api.createAccount(channel.provider, values)
            setCreateVisible(false)
            load()
            onChanged()
          }}
        />
      )}
      {editingAccount && (
        <AccountFormModal
          provider={channel.provider}
          existing={editingAccount}
          onClose={() => setEditingAccount(null)}
          onSubmit={async (values) => {
            await api.updateAccount(channel.provider, editingAccount.id, values)
            setEditingAccount(null)
            load()
            onChanged()
          }}
        />
      )}
    </div>
  )
}

// ---- 模型路由面板 ----

function ModelsPanel({ channel, onChanged }: { channel: Channel; onChanged: () => void }) {
  const [modelFormVisible, setModelFormVisible] = useState(false)
  const [editingModel, setEditingModel] = useState<ChannelModel | null>(null)

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'flex-end', marginBottom: 8 }}>
        <Button
          type="primary"
          theme="solid"
          onClick={() => {
            setEditingModel(null)
            setModelFormVisible(true)
          }}
        >
          添加模型
        </Button>
      </div>
      <Table
        size="small"
        pagination={false}
        dataSource={channel.models ?? []}
        rowKey="name"
        columns={[
          { title: '模型', dataIndex: 'name', width: 160 },
          { title: '协议路由', width: 110, render: (_: unknown, m: ChannelModel) => Object.keys(m.routes ?? {}).map((k) => k.replace('/v1/', '')).join('、') || '-' },
          { title: '输入/输出 ¥/M', width: 110, render: (_: unknown, m: ChannelModel) => `${m.pricing?.input_per_m ?? 0}/${m.pricing?.output_per_m ?? 0}` },
          { title: '缓存读 ¥/M', width: 100, render: (_: unknown, m: ChannelModel) => String(m.pricing?.cache_read_per_m ?? 0) },
          {
            title: '启用',
            dataIndex: 'enabled',
            width: 70,
            render: (v: boolean) => (v ? <Tag color="green">是</Tag> : <Tag color="grey">否</Tag>),
          },
          {
            title: '操作',
            width: 150,
            render: (_: unknown, m: ChannelModel) => (
              <div style={{ display: 'flex', gap: 6 }}>
                <Button
                  size="small"
                  onClick={async () => {
                    const r = await api.testChannel(channel.provider, m.name)
                    r.ok ? Toast.success(`模型测试通过（${r.latency_ms}ms）`) : Toast.error(`失败：${r.error ?? r.status}`)
                  }}
                >
                  测试
                </Button>
                <Button
                  size="small"
                  onClick={() => {
                    setEditingModel(m)
                    setModelFormVisible(true)
                  }}
                >
                  编辑
                </Button>
                <Popconfirm
                  title={`删除模型 ${m.name}？`}
                  onConfirm={async () => {
                    await api.deleteModel(channel.provider, m.name)
                    onChanged()
                  }}
                >
                  <Button size="small" type="danger">
                    删除
                  </Button>
                </Popconfirm>
              </div>
            ),
          },
        ]}
        empty="该渠道还没有模型"
      />

      {modelFormVisible && (
        <ModelFormModal
          provider={channel.provider}
          existing={editingModel}
          onClose={() => setModelFormVisible(false)}
          onSubmit={async (values) => {
            if (editingModel) await api.updateModel(channel.provider, editingModel.name, values)
            else await api.createModel(channel.provider, values)
            setModelFormVisible(false)
            onChanged()
          }}
        />
      )}
    </div>
  )
}

// ---- 各表单 Modal（渠道 / 凭据 / 模型 / 账号 / 批量导入） ----

function ChannelFormModal({
  existing,
  onClose,
  onSubmit,
}: {
  existing: Channel | null
  onClose: () => void
  onSubmit: (values: Partial<Channel>) => Promise<void>
}) {
  const [saving, setSaving] = useState(false)
  return (
    <Modal title={existing ? `编辑渠道 ${existing.name}` : '新建渠道'} visible onClose={onClose} footer={null}>
      <Form
        initValues={existing ?? { provider: '', name: '', auth_mode: 'bearer', balance_type: 'balance', balance_url: '', models_url: '' }}
        onSubmit={async (values) => {
          setSaving(true)
          try {
            await onSubmit(values)
          } finally {
            setSaving(false)
          }
        }}
      >
        <Form.Input field="provider" label="Provider 标识" placeholder="如 deepseek" rules={[{ required: true, message: '必填' }]} disabled={!!existing} />
        <Form.Input field="name" label="显示名称" rules={[{ required: true, message: '必填' }]} />
        <Form.Select field="auth_mode" label="上游认证模式" style={{ width: 200 }}>
          <Select.Option value="bearer">bearer（Authorization: Bearer）</Select.Option>
          <Select.Option value="x_api_key">x_api_key（x-api-key）</Select.Option>
        </Form.Select>
        <Form.Select field="balance_type" label="余额类型" style={{ width: 200 }}>
          <Select.Option value="balance">balance（金额）</Select.Option>
          <Select.Option value="quota">quota（余量百分比）</Select.Option>
        </Form.Select>
        <Form.Input field="balance_url" label="余额查询 URL" placeholder="可选" />
        <Form.Input field="models_url" label="远端模型列表 URL" placeholder="可选" />
        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 12 }}>
          <Button onClick={onClose}>取消</Button>
          <Button htmlType="submit" type="primary" theme="solid" loading={saving}>
            保存
          </Button>
        </div>
      </Form>
    </Modal>
  )
}

function SetKeyModal({ channel, onClose, onSubmit }: { channel: Channel; onClose: () => void; onSubmit: (key: string) => Promise<void> }) {
  const [key, setKey] = useState('')
  const [saving, setSaving] = useState(false)
  return (
    <Modal title={`更新 ${channel.name} 上游凭据`} visible onClose={onClose} footer={null}>
      <div style={{ marginBottom: 12 }}>
        <Text type="tertiary" size="small">
          当前：{channel.key_prefix || '未配置'}。凭据 AES-GCM 加密落库；若该渠道未配置显式账号，此凭据同时作为隐式账号参与转发。
        </Text>
      </div>
      <Input mode="password" placeholder="粘贴上游 API key" value={key} onChange={setKey} />
      <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 12 }}>
        <Button onClick={onClose}>取消</Button>
        <Button
          type="primary"
          theme="solid"
          disabled={!key}
          loading={saving}
          onClick={async () => {
            setSaving(true)
            try {
              await onSubmit(key)
            } finally {
              setSaving(false)
            }
          }}
        >
          保存
        </Button>
      </div>
    </Modal>
  )
}

interface ModelFormValues {
  name: string
  enabled: boolean
  pricing: { input_per_m: number; output_per_m: number; cache_read_per_m: number; cache_write_per_m: number }
  routes_json: string
}

function ModelFormModal({
  provider,
  existing,
  onClose,
  onSubmit,
}: {
  provider: string
  existing: ChannelModel | null
  onClose: () => void
  onSubmit: (values: Partial<ChannelModel>) => Promise<void>
}) {
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const defaultRoutes = existing?.routes ?? {
    '/v1/messages': { upstream: '', model: '', usage: 'anthropic' },
    '/v1/chat/completions': { upstream: '', model: '', usage: 'chat_completions' },
    '/v1/responses': { upstream: '', model: '', usage: 'responses' },
  }
  return (
    <Modal title={existing ? `编辑模型 ${existing.name}` : `为 ${provider} 添加模型`} visible onClose={onClose} footer={null} width={560}>
      <Form<ModelFormValues>
        initValues={
          {
            ...existing,
            pricing: existing?.pricing ?? { input_per_m: 0, output_per_m: 0, cache_read_per_m: 0, cache_write_per_m: 0 },
            routes_json: JSON.stringify(defaultRoutes, null, 2),
          } as unknown as ModelFormValues
        }
        onSubmit={async (values) => {
          setSaving(true)
          setError('')
          try {
            const routes = JSON.parse(values.routes_json || '{}')
            await onSubmit({ ...values, routes })
          } catch (e) {
            setError(`routes 不是合法 JSON：${(e as Error).message}`)
          } finally {
            setSaving(false)
          }
        }}
      >
        <Form.Input field="name" label="客户端模型名" rules={[{ required: true, message: '必填' }]} disabled={!!existing} />
        <Form.Input field="pricing.input_per_m" label="输入 ¥/M" />
        <Form.Input field="pricing.output_per_m" label="输出 ¥/M" />
        <Form.Input field="pricing.cache_read_per_m" label="缓存读 ¥/M" />
        <Form.Input field="pricing.cache_write_per_m" label="缓存写 ¥/M" />
        <Form.Switch field="enabled" label="启用" initValue={existing?.enabled ?? true} />
        <Form.TextArea
          field="routes_json"
          label="协议路由（JSON）"
          initValue={JSON.stringify(defaultRoutes, null, 2)}
          rules={[{ required: true, message: '必填' }]}
          style={{ fontFamily: 'monospace', fontSize: 12 }}
          rows={10}
        />
        {error && <Text type="danger" size="small">{error}</Text>}
        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 12 }}>
          <Button onClick={onClose}>取消</Button>
          <Button htmlType="submit" type="primary" theme="solid" loading={saving}>
            保存
          </Button>
        </div>
      </Form>
    </Modal>
  )
}

interface AccountFormValues {
  name: string
  key?: string
  max_concurrency: number
  enabled?: boolean
}

function AccountFormModal({
  provider,
  existing,
  onClose,
  onSubmit,
}: {
  provider: string
  existing: UpstreamAccount | null
  onClose: () => void
  onSubmit: (values: { name: string; key: string; max_concurrency: number; enabled?: boolean }) => Promise<void>
}) {
  const [saving, setSaving] = useState(false)
  return (
    <Modal title={existing ? `编辑账号 ${existing.name}` : `为 ${provider} 添加上游账号`} visible onClose={onClose} footer={null}>
      <Form<AccountFormValues>
        initValues={{ name: existing?.name ?? '', max_concurrency: existing?.max_concurrency ?? 0, enabled: existing?.enabled ?? true }}
        onSubmit={async (values) => {
          setSaving(true)
          try {
            await onSubmit({
              name: values.name,
              key: values.key || '',
              max_concurrency: Number(values.max_concurrency) || 0,
              enabled: values.enabled,
            })
          } finally {
            setSaving(false)
          }
        }}
      >
        <Form.Input field="name" label="账号名称" placeholder="如 account-a" rules={[{ required: true, message: '必填' }]} />
        <Form.Input field="key" label={existing ? '上游凭据（留空 = 保留原值）' : '上游凭据'} mode="password" rules={existing ? [] : [{ required: true, message: '必填' }]} />
        <Form.Input field="max_concurrency" label="最大并发（0 = 不限）" />
        {existing && <Form.Switch field="enabled" label="启用" />}
        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 12 }}>
          <Button onClick={onClose}>取消</Button>
          <Button htmlType="submit" type="primary" theme="solid" loading={saving}>
            保存
          </Button>
        </div>
      </Form>
    </Modal>
  )
}

// 批量导入：每行一个 key，可选 "名称:key" 前缀
function ImportAccountsModal({
  provider,
  existingCount,
  onClose,
  onDone,
}: {
  provider: string
  existingCount: number
  onClose: () => void
  onDone: () => void
}) {
  const [text, setText] = useState('')
  const [maxConcurrency, setMaxConcurrency] = useState('0')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  const lines = text.split('\n').map((l) => l.trim()).filter(Boolean)
  const items = lines.map((line, i) => {
    const sep = line.indexOf(':')
    if (sep > 0 && !line.startsWith('http')) {
      return { name: line.slice(0, sep), key: line.slice(sep + 1), max_concurrency: Number(maxConcurrency) || 0 }
    }
    return { name: `account-${existingCount + i + 1}`, key: line, max_concurrency: Number(maxConcurrency) || 0 }
  })

  return (
    <Modal title={`批量导入账号到 ${provider}`} visible onClose={onClose} footer={null} width={560}>
      <div style={{ marginBottom: 8 }}>
        <Text type="tertiary" size="small">
          每行一个凭据；可用「名称:凭据」给账号命名。重复凭据自动跳过。
        </Text>
      </div>
      <TextArea value={text} onChange={setText} rows={8} placeholder={'account-a:sk-xxxx\naccount-b:sk-yyyy\nsk-zzzz'} style={{ fontFamily: 'monospace', fontSize: 12 }} />
      <div style={{ margin: '12px 0' }}>
        <Text size="small" style={{ marginRight: 8 }}>
          统一最大并发（0 = 不限）：
        </Text>
        <Input value={maxConcurrency} onChange={setMaxConcurrency} style={{ width: 120 }} />
        <Text type="tertiary" size="small" style={{ marginLeft: 12 }}>
          将导入 {items.length} 个
        </Text>
      </div>
      {error && <Text type="danger" size="small">{error}</Text>}
      <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8 }}>
        <Button onClick={onClose}>取消</Button>
        <Button
          type="primary"
          theme="solid"
          disabled={items.length === 0}
          loading={saving}
          onClick={async () => {
            setSaving(true)
            setError('')
            try {
              const r = await api.importAccounts(provider, items)
              Toast.success(`导入完成：新增 ${r.added}，重复跳过 ${r.duplicated}`)
              onDone()
            } catch (e) {
              setError(String((e as Error).message ?? e))
            } finally {
              setSaving(false)
            }
          }}
        >
          导入
        </Button>
      </div>
    </Modal>
  )
}
