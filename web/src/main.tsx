import { LocaleProvider } from '@douyinfe/semi-ui'
import zhCN from '@douyinfe/semi-ui/lib/es/locale/source/zh_CN'
import { StrictMode, useEffect, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { HashRouter, Navigate, Route, Routes, useNavigate } from 'react-router-dom'
import { getToken, setToken } from './api/client'
import './index.css'
import AdminLayout from './layouts/AdminLayout'
import Channels from './pages/Channels'
import Dashboard from './pages/Dashboard'
import Keys from './pages/Keys'
import Login from './pages/Login'
import Logs from './pages/Logs'
import Settings from './pages/Settings'
import Usage from './pages/Usage'

// 401 事件 → 清 token 回登录页（api/client.ts 里触发）
function UnauthorizedWatcher() {
  const navigate = useNavigate()
  useEffect(() => {
    const handler = () => navigate('/login')
    window.addEventListener('relaydock:unauthorized', handler)
    return () => window.removeEventListener('relaydock:unauthorized', handler)
  }, [navigate])
  return null
}

function RequireAuth({ children }: { children: React.ReactElement }) {
  const [, tick] = useState(0)
  useEffect(() => {
    const h = () => tick((n) => n + 1)
    window.addEventListener('relaydock:unauthorized', h)
    return () => window.removeEventListener('relaydock:unauthorized', h)
  }, [])
  if (!getToken()) return <Navigate to="/login" replace />
  return children
}

function App() {
  return (
    <StrictMode>
      <LocaleProvider locale={zhCN}>
        <HashRouter>
          <UnauthorizedWatcher />
          <Routes>
            <Route path="/login" element={<Login />} />
            <Route
              path="/"
              element={
                <RequireAuth>
                  <AdminLayout />
                </RequireAuth>
              }
            >
              <Route index element={<Dashboard />} />
              <Route path="usage" element={<Usage />} />
              <Route path="logs" element={<Logs />} />
              <Route path="keys" element={<Keys />} />
              <Route path="channels" element={<Channels />} />
              <Route path="settings" element={<Settings />} />
            </Route>
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </HashRouter>
      </LocaleProvider>
    </StrictMode>
  )
}

// 登出辅助（顶栏用）
export function logout() {
  setToken('')
  window.dispatchEvent(new Event('relaydock:unauthorized'))
}

createRoot(document.getElementById('root')!).render(<App />)
