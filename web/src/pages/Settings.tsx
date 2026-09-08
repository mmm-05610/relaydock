import { Banner, Button, Card, Descriptions, Form, Input, Modal, Switch, Tag, Typography } from '@douyinfe/semi-ui'
import { useEffect, useState } from 'react'
import { Toast } from '@douyinfe/semi-ui'
import { api, setToken } from '../api/client'

const { Title, Text } = Typography

export default function Settings() {
  const [upstream, setUpstream] = useState<Record<string, boolean>>({})
  const [balances, setBalances] = useState<Record<string, Record<string, unknown>> | null>(null)
  const [logCapture, setLogCapture] = useState(false)
  const [retention, setRetention] = useState('7')
  const [error, setError] = useState('')
  const [passwordVisible, setPasswordVisible] = useState(false)

  const load = async () => {
    try {
      setUpstream(await api.upstreamStatus())
      setError('')
    } catch (e) {
      setError(String((e as Error).message ?? e))
    }
  }

  useEffect(() => {
    load()
    api.getLoggingSettings().then((r) => {
      setLogCapture(r.enabled)
      setRetention(String(r.retention_days))
    })
  }, [])

  return (
    <div>
      <Title heading={5} style={{ marginBottom: 16 }}>
        设置
      </Title>
      {error && <Banner type="danger" description={error} style={{ marginBottom: 12 }} />}

      <Card title="上游凭据状态" style={{ marginBottom: 16 }} bodyStyle={{ padding: 16 }}>
        {Object.keys(upstream).length === 0 ? (
          <Text type="tertiary">暂无渠道</Text>
        ) : (
          <Descriptions
            row
            data={Object.entries(upstream).map(([provider, ok]) => ({
              key: provider,
              value: ok ? <Tag color="green">已配置</Tag> : <Tag color="red">未配置</Tag>,
            }))}
          />
        )}
        <div style={{ display: 'flex', gap: 8, marginTop: 12 }}>
          <Button size="small" onClick={load}>
            刷新
          </Button>
          <Button
            size="small"
            onClick={async () => {
              const r = await api.upstreamBalance()
              setBalances(r)
            }}
          >
            查询各渠道余额
          </Button>
        </div>
        {balances && (
          <div style={{ marginTop: 12 }}>
            {Object.entries(balances).map(([provider, info]) => (
              <div key={provider} style={{ marginBottom: 8 }}>
                <Text strong>{provider}：</Text>
                <Text code size="small">
                  {JSON.stringify(info)}
                </Text>
              </div>
            ))}
          </div>
        )}
      </Card>

      <Card title="全文请求/响应日志" style={{ marginBottom: 16 }} bodyStyle={{ padding: 16 }}>
        <Text type="tertiary" size="small" style={{ display: 'block', marginBottom: 12 }}>
          开启后记录每个请求的完整请求体与响应体（观测旁路，默认关闭）。内容含对话等敏感信息，仅管理口令可见；按保留期自动清理。
        </Text>
        <div style={{ display: 'flex', gap: 12, alignItems: 'center' }}>
          <Switch checked={logCapture} onChange={async (v) => {
            const r = (await api.updateLoggingSettings({ enabled: v as boolean })) as { enabled: boolean; retention_days: number }
            setLogCapture(r.enabled)
            Toast.success(v ? '已开启' : '已关闭')
          }} />
          <Text>启用记录</Text>
          <Text size="small" style={{ marginLeft: 16 }}>保留天数：</Text>
          <Input value={retention} onChange={setRetention} style={{ width: 100 }} />
          <Button size="small" onClick={async () => {
            const r = (await api.updateLoggingSettings({ retention_days: Number(retention) || 7 })) as { enabled: boolean; retention_days: number }
            setRetention(String(r.retention_days))
            Toast.success('已保存')
          }}>保存</Button>
        </div>
      </Card>

      <Card title="面板口令" style={{ marginBottom: 16 }} bodyStyle={{ padding: 16 }}>
        <Text type="tertiary" size="small" style={{ display: 'block', marginBottom: 12 }}>
          运行中修改即时生效，重启后恢复为环境变量 PANEL_PASSWORD。
        </Text>
        <Button onClick={() => setPasswordVisible(true)}>修改口令</Button>
      </Card>

      <Card title="关于" bodyStyle={{ padding: 16 }}>
        <Descriptions
          row
          size="small"
          data={[
            { key: '形态', value: 'Go 单二进制 + 静态控制台（本页）' },
            { key: '原则', value: '纯透传，不做协议转换；计量为旁路' },
            { key: '凭据安全', value: '上游 key AES-256-GCM 加密落库，面板不回显明文' },
          ]}
        />
      </Card>

      {passwordVisible && (
        <PasswordModal
          onClose={() => setPasswordVisible(false)}
          onSubmit={async (password) => {
            await api.updatePassword(password)
            setToken(password) // 新口令即时生效
            setPasswordVisible(false)
            Toast.success('口令已更新')
          }}
        />
      )}
    </div>
  )
}

function PasswordModal({ onClose, onSubmit }: { onClose: () => void; onSubmit: (password: string) => Promise<void> }) {
  const [saving, setSaving] = useState(false)
  return (
    <Modal title="修改面板口令" visible onCancel={onClose} footer={null}>
      <Form
        onSubmit={async (values) => {
          setSaving(true)
          try {
            await onSubmit((values as { password: string }).password)
          } finally {
            setSaving(false)
          }
        }}
      >
        <Form.Input field="password" label="新口令" mode="password" rules={[{ required: true, message: '必填' }]} />
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
