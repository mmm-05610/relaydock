import { Button, Card, Form, Typography } from '@douyinfe/semi-ui'
import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api, ApiError, setToken } from '../api/client'

const { Title, Text } = Typography

export default function Login() {
  const navigate = useNavigate()
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  const onSubmit = async (values: { password: string }) => {
    setLoading(true)
    setError('')
    try {
      setToken(values.password)
      await api.verify() // 401 会清 token 并抛错
      navigate('/', { replace: true })
    } catch (e) {
      setToken('')
      setError(
        e instanceof ApiError && e.status === 401
          ? '口令不正确'
          : `无法连接网关（${e instanceof Error ? e.message : e}），请确认 gateway 已在 :8080 运行`,
      )
    } finally {
      setLoading(false)
    }
  }

  return (
    <div
      style={{
        height: '100vh',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        backgroundColor: 'var(--semi-color-bg-0)',
      }}
    >
      <Card style={{ width: 380 }}>
        <Title heading={4} style={{ marginBottom: 4 }}>
          RelayDock 控制台
        </Title>
        <Text type="tertiary">输入管理口令（PANEL_PASSWORD）登录</Text>
        <Form onSubmit={onSubmit} style={{ marginTop: 16 }}>
          <Form.Input
            field="password"
            label="管理口令"
            mode="password"
            rules={[{ required: true, message: '请输入口令' }]}
          />
          {error && (
            <Text type="danger" size="small">
              {error}
            </Text>
          )}
          <Button htmlType="submit" type="primary" theme="solid" block loading={loading} style={{ marginTop: 12 }}>
            登录
          </Button>
        </Form>
      </Card>
    </div>
  )
}
