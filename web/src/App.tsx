import { Navigate, Route, Routes } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { ToastProvider } from '@/components/ui/toast'
import { Layout } from '@/components/Layout'
import { ProvidersPage } from '@/pages/Providers'
import { ModelsPage } from '@/pages/Models'
import { SettingsPage } from '@/pages/Settings'
import { LogsPage } from '@/pages/Logs'
import { DataLocationPage } from '@/pages/DataLocation'
import { LoginPage } from '@/pages/Login'

export default function App() {
  const session = useQuery({ queryKey: ['session'], queryFn: api.session, retry: 0 })

  if (session.isLoading) {
    return <div className="flex h-full items-center justify-center text-sm text-slate-500">加载中…</div>
  }

  if (session.isError || !session.data) {
    const message = session.error instanceof Error ? session.error.message : '未知错误'
    return (
      <div className="flex h-full flex-col items-center justify-center gap-2 text-sm">
        <p className="font-medium text-red-600">无法连接网关</p>
        <p className="text-slate-500">{message}</p>
        <p className="text-slate-400">请确认 agoramodel 正在运行，且页面与 API 同源访问。</p>
      </div>
    )
  }

  const info = session.data

  if (info.login_required && !info.authenticated) {
    return (
      <ToastProvider>
        <LoginPage version={info.version} />
      </ToastProvider>
    )
  }

  return (
    <ToastProvider>
      <Layout version={info.version} loginRequired={info.login_required}>
        <Routes>
          <Route path="/" element={<Navigate to="/providers" replace />} />
          <Route path="/providers" element={<ProvidersPage />} />
          <Route path="/models" element={<ModelsPage />} />
          <Route path="/settings" element={<SettingsPage />} />
          <Route path="/logs" element={<LogsPage />} />
          <Route path="/data" element={<DataLocationPage />} />
          <Route path="*" element={<Navigate to="/providers" replace />} />
        </Routes>
      </Layout>
    </ToastProvider>
  )
}
