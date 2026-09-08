import { IconBeaker, IconHistogram, IconList, IconSetting, IconKey, IconServer } from '@douyinfe/semi-icons'
import { Avatar, Layout, Nav, Typography } from '@douyinfe/semi-ui'
import { Outlet, useLocation, useNavigate } from 'react-router-dom'
import { logout } from '../main'

const { Sider, Header, Content } = Layout
const { Text } = Typography

const MENUS = [
  { itemKey: '/', text: '概览', icon: <IconHistogram /> },
  { itemKey: '/usage', text: '用量分析', icon: <IconBeaker /> },
  { itemKey: '/logs', text: '请求日志', icon: <IconList /> },
  { itemKey: '/keys', text: '虚拟 Key', icon: <IconKey /> },
  { itemKey: '/channels', text: '渠道与账号', icon: <IconServer /> },
  { itemKey: '/settings', text: '设置', icon: <IconSetting /> },
]

const TITLES: Record<string, string> = Object.fromEntries(MENUS.map((m) => [m.itemKey, m.text]))

export default function AdminLayout() {
  const navigate = useNavigate()
  const { pathname } = useLocation()
  const selected = MENUS.find((m) => (m.itemKey === '/' ? pathname === '/' : pathname.startsWith(m.itemKey)))?.itemKey ?? '/'

  return (
    <Layout style={{ height: '100vh' }}>
      <Sider style={{ backgroundColor: 'var(--semi-color-bg-1)' }}>
        <Nav
          style={{ height: '100%' }}
          selectedKeys={[selected]}
          items={MENUS}
          header={{ text: <Text strong style={{ fontSize: 16 }}>RelayDock 控制台</Text> }}
          footer={{ collapseButton: true }}
          onSelect={(data) => navigate(String(data.itemKey))}
        />
      </Sider>
      <Layout>
        <Header
          style={{
            display: 'flex',
            justifyContent: 'flex-end',
            alignItems: 'center',
            gap: 12,
            backgroundColor: 'var(--semi-color-bg-1)',
            padding: '8px 24px',
            borderBottom: '1px solid var(--semi-color-border)',
          }}
        >
          <Text type="tertiary">{TITLES[selected] ?? ''}</Text>
          <Avatar size="small" alt="admin" style={{ backgroundColor: 'var(--semi-color-primary)' }}>
            A
          </Avatar>
          <Text link onClick={logout} style={{ cursor: 'pointer' }}>
            退出
          </Text>
        </Header>
        <Content style={{ padding: 24, overflow: 'auto', backgroundColor: 'var(--semi-color-bg-0)' }}>
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  )
}
