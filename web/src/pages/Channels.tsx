import {
  Banner,
  Button,
  Card,
  Form,
  Input,
  Modal,
  Popconfirm,
  Select,
  Table,
  Tag,
  Typography,
} from '@douyinfe/semi-ui'
import { useCallback, useEffect, useState } from 'react'
import { Toast } from '@douyinfe/semi-ui'
import { api, type Channel, type ChannelModel, type UpstreamAccount } from '../api/client'

const { Title, Text } = Typography

export default function Channels() {
  const [channels, setChannels] = useState<Channel[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [editing, setEditing] = useState<Channel | null>(null) // null = 新建
  const [createVisible, setCreateVisible] = useState(false)
  const [keyTarget, setKeyTarget] = useState<Channel | null>(null)

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
              width: 320,
              render: (_: unknown, ch: Channel) => (
                <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
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
          expandedRowRender={(ch?: Channel) => (ch ? <ChannelDetail channel={ch} onChanged={load} /> : null)}
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
    </div>
  )
}

// ---- 渠道展开详情：模型路由 + 上游账号池 ----

function ChannelDetail({ channel, onChanged }: { channel: Channel; onChanged: () => void }) {
  const [accounts, setAccounts] = useState<UpstreamAccount[]>([])
  const [modelFormVisible, setModelFormVisible] = useState(false)
  const [editingModel, setEditingModel] = useState<ChannelModel | null>(null)
  const [accountFormVisible, setAccountFormVisible] = useState(false)
  const [editingAccount, setEditingAccount] = useState<UpstreamAccount | null>(null)

  const loadAccounts = useCallback(async () => {
    try {
      setAccounts(await api.accounts(channel.provider))
    } catch {
      setAccounts([])
    }
  }, [channel.provider])

  useEffect(() => {
    loadAccounts()
  }, [loadAccounts])

  return (
    <div style={{ padding: '4px 12px 12px', display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
      {/* 模型路由 */}
      <div>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
          <Text strong>模型路由</Text>
          <Button
            size="small"
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
            { title: '模型', dataIndex: 'name', width: 150 },
            { title: '路由', width: 70, render: (_: unknown, m: ChannelModel) => Object.keys(m.routes ?? {}).length },
            { title: '输入/输出 ¥/M', width: 110, render: (_: unknown, m: ChannelModel) => `${m.pricing?.input_per_m ?? 0}/${m.pricing?.output_per_m ?? 0}` },
            {
              title: '启用',
              dataIndex: 'enabled',
              width: 70,
              render: (v: boolean) => (v ? <Tag color="green">是</Tag> : <Tag color="grey">否</Tag>),
            },
            {
              title: '操作',
              width: 130,
              render: (_: unknown, m: ChannelModel) => (
                <div style={{ display: 'flex', gap: 6 }}>
                  <Button
                    size="small"
                    onClick={async () => {
                      const r = await api.testChannel(channel.provider, m.name)
                      r.ok ? Toast.success(`模型测试通过（${r.latency_ms}ms）`) : Toast.error(`失败：${r.error ?? r.status}`)
                    }}
                  >
                    测
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
                      删
                    </Button>
                  </Popconfirm>
                </div>
              ),
            },
          ]}
          empty="该渠道还没有模型"
        />
      </div>

      {/* 上游账号池 */}
      <div>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
          <Text strong>上游账号池</Text>
          <Button
            size="small"
            onClick={() => {
              setEditingAccount(null)
              setAccountFormVisible(true)
            }}
          >
            添加账号
          </Button>
        </div>
        <Table
          size="small"
          pagination={false}
          dataSource={accounts}
          rowKey="id"
          columns={[
            { title: '账号', dataIndex: 'name', width: 110 },
            {
              title: '状态',
              dataIndex: 'state',
              width: 90,
              render: (v: string) =>
                v === 'healthy' ? <Tag color="green">健康</Tag> : v === 'cooling' ? <Tag color="orange">冷却中</Tag> : <Tag color="grey">禁用</Tag>,
            },
            {
              title: '并发',
              width: 80,
              render: (_: unknown, a: UpstreamAccount) => `${a.inflight}/${a.max_concurrency || '∞'}`,
            },
            {
              title: '冷却至',
              dataIndex: 'cooling_until',
              width: 110,
              render: (v: string | null) => (v ? new Date(v).toLocaleTimeString('zh-CN') : '-'),
            },
            {
              title: '操作',
              width: 140,
              render: (_: unknown, a: UpstreamAccount) => (
                <div style={{ display: 'flex', gap: 6 }}>
                  <Button
                    size="small"
                    onClick={async () => {
                      const r = await api.testAccount(channel.provider, a.id)
                      r.ok ? Toast.success(`账号测试通过（${r.latency_ms}ms）`) : Toast.error(`失败：${r.error ?? r.status}`)
                    }}
                  >
                    测
                  </Button>
                  <Button
                    size="small"
                    onClick={() => {
                      setEditingAccount(a)
                      setAccountFormVisible(true)
                    }}
                  >
                    编辑
                  </Button>
                  <Popconfirm
                    title={`删除账号 ${a.name}？在途请求不受影响。`}
                    onConfirm={async () => {
                      await api.deleteAccount(channel.provider, a.id)
                      onChanged()
                      loadAccounts()
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
              未配置显式账号 —— 当前走「渠道凭据」单 key 路径，行为不变
            </Text>
          }
        />
      </div>

      {/* 模型表单 */}
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
      {/* 账号表单 */}
      {accountFormVisible && (
        <AccountFormModal
          provider={channel.provider}
          existing={editingAccount}
          onClose={() => setAccountFormVisible(false)}
          onSubmit={async (values) => {
            if (editingAccount) await api.updateAccount(channel.provider, editingAccount.id, values)
            else await api.createAccount(channel.provider, values as { name: string; key: string; max_concurrency: number })
            setAccountFormVisible(false)
            onChanged()
            loadAccounts()
          }}
        />
      )}
    </div>
  )
}

// ---- 渠道表单 ----

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
        initValues={
          existing ?? { provider: '', name: '', auth_mode: 'bearer', balance_type: 'balance', balance_url: '', models_url: '' }
        }
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

// ---- 模型表单（routes 用 JSON 编辑，面向高级配置） ----

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

// ---- 账号表单 ----

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
