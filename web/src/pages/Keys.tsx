import { Banner, Button, Card, Form, Modal, Popconfirm, Progress, Table, Tag, Typography } from '@douyinfe/semi-ui'
import { useCallback, useEffect, useState } from 'react'
import { api, type VirtualKey } from '../api/client'

const { Title, Text } = Typography

export default function Keys() {
  const [keys, setKeys] = useState<VirtualKey[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [createVisible, setCreateVisible] = useState(false)
  const [editTarget, setEditTarget] = useState<VirtualKey | null>(null)
  const [issuedKey, setIssuedKey] = useState('') // 新建/轮换后仅此一次可见的明文

  const load = useCallback(async () => {
    setLoading(true)
    try {
      setKeys(await api.listKeys())
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

  const showIssued = (raw: string) => {
    setIssuedKey(raw)
    load()
  }

  const copy = (text: string) => navigator.clipboard.writeText(text)

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
        <Title heading={5}>虚拟 Key</Title>
        <Button theme="solid" type="primary" onClick={() => setCreateVisible(true)}>
          新建 Key
        </Button>
      </div>

      {error && <Banner type="danger" description={error} style={{ marginBottom: 12 }} />}
      {issuedKey && (
        <Banner
          type="success"
          closeIcon={null}
          style={{ marginBottom: 12 }}
          description={
            <div>
              <div>新 key 仅此一次可见，请立即保存：</div>
              <Text code copyable={{ onCopy: () => copy(issuedKey) }}>
                {issuedKey}
              </Text>
            </div>
          }
          onClose={() => setIssuedKey('')}
        />
      )}

      <Card bodyStyle={{ paddingTop: 8 }}>
        <Table
          size="small"
          loading={loading}
          dataSource={keys}
          rowKey="key_hash"
          columns={[
            { title: '名称', dataIndex: 'name', width: 140 },
            { title: '归属', dataIndex: 'owner', width: 110, render: (v: string) => v || '-' },
            { title: 'Agent', dataIndex: 'agent_type', width: 110, render: (v: string) => v || '-' },
            {
              title: '额度（¥ 等效成本）',
              width: 190,
              render: (_: unknown, k: VirtualKey) =>
                k.quota_limit > 0 ? (
                  <div style={{ width: 170 }}>
                    <Text size="small">
                      {k.quota_used.toFixed(3)} / {k.quota_limit.toFixed(2)}
                    </Text>
                    <Progress percent={(k.quota_used / k.quota_limit) * 100} showInfo style={{ marginTop: 4 }} />
                  </div>
                ) : (
                  <>
                    已用 {k.quota_used.toFixed(3)}
                    <Text type="tertiary">（不限）</Text>
                  </>
                ),
            },
            {
              title: '允许模型',
              dataIndex: 'allowed_models',
              width: 180,
              render: (v: string) =>
                v ? (
                  <Text ellipsis={{ showTooltip: true }} style={{ maxWidth: 170 }} code size="small">
                    {v}
                  </Text>
                ) : (
                  <Text type="tertiary">不限</Text>
                ),
            },
            {
              title: '状态',
              dataIndex: 'enabled',
              width: 90,
              render: (v: boolean) => (v ? <Tag color="green">启用</Tag> : <Tag color="red">已吊销</Tag>),
            },
            {
              title: '操作',
              width: 200,
              render: (_: unknown, k: VirtualKey) => (
                <div style={{ display: 'flex', gap: 8 }}>
                  <Button size="small" onClick={() => setEditTarget(k)}>
                    编辑
                  </Button>
                  <Popconfirm
                    title="轮换后旧 key 立即失效，确定？"
                    onConfirm={async () => {
                      const r = await api.rotateKey(k.key_hash)
                      showIssued(r.key)
                    }}
                  >
                    <Button size="small">轮换</Button>
                  </Popconfirm>
                  {k.enabled && (
                    <Popconfirm title="吊销后使用该 key 的 agent 将立即 401，确定？" onConfirm={async () => { await api.revokeKey(k.key_hash); load() }}>
                      <Button size="small" type="danger">
                        吊销
                      </Button>
                    </Popconfirm>
                  )}
                </div>
              ),
            },
          ]}
          empty="还没有虚拟 Key，点右上角「新建」签发一个"
        />
      </Card>

      <KeyFormModal
        visible={createVisible}
        onClose={() => setCreateVisible(false)}
        onSubmit={async (values) => {
          const r = await api.createKey({ ...values, quota: Number(values.quota) || 0 })
          setCreateVisible(false)
          showIssued(r.key)
        }}
      />
      {editTarget && (
        <KeyFormModal
          visible
          existing={editTarget}
          onClose={() => setEditTarget(null)}
          onSubmit={async (values) => {
            await api.updateKey(editTarget.key_hash, { ...values, quota: Number(values.quota) || 0 })
            setEditTarget(null)
            load()
          }}
        />
      )}
    </div>
  )
}

type KeyForm = { name: string; owner: string; agent_type: string; quota: number; allowed_models: string }

function KeyFormModal({
  visible,
  existing,
  onClose,
  onSubmit,
}: {
  visible: boolean
  existing?: VirtualKey
  onClose: () => void
  onSubmit: (values: KeyForm) => Promise<void>
}) {
  const [saving, setSaving] = useState(false)
  return (
    <Modal title={existing ? `编辑 ${existing.name}` : '新建虚拟 Key'} visible={visible} onCancel={onClose} footer={null} closeOnEsc>
      <Form<KeyForm>
        initValues={
          existing
            ? { name: existing.name, owner: existing.owner, agent_type: existing.agent_type, quota: existing.quota_limit, allowed_models: existing.allowed_models }
            : { name: '', owner: '', agent_type: 'claude-code', quota: 0, allowed_models: '' }
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
        <Form.Input field="name" label="名称" placeholder="如 claude-code-main" rules={[{ required: true, message: '必填' }]} />
        <Form.Input field="owner" label="归属人" placeholder="可选" />
        <Form.Input field="agent_type" label="Agent 类型" placeholder="claude-code / codex / opencode" />
        <Form.Input field="quota" label="额度上限" placeholder="0 = 不限" />
        <Form.Input field="allowed_models" label="允许模型（逗号分隔）" placeholder="留空 = 不限，如 deepseek-v4-pro,minimax-m3" />
        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 12 }}>
          <Button onClick={onClose}>取消</Button>
          <Button htmlType="submit" type="primary" theme="solid" loading={saving}>
            {existing ? '保存' : '创建'}
          </Button>
        </div>
      </Form>
    </Modal>
  )
}
