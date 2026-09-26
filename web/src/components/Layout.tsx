import type { ReactNode } from 'react'
import { NavLink } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { Boxes, LogOut, Radio, ScrollText, Server } from 'lucide-react'
import { api } from '@/lib/api'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'

const NAV_ITEMS = [
  { to: '/providers', label: '供应商管理', icon: Server },
  { to: '/models', label: '模型列表', icon: Boxes },
  { to: '/settings', label: '网关设置', icon: Radio },
  { to: '/logs', label: '请求日志', icon: ScrollText },
]

export function Layout({
  version,
  loginRequired,
  children,
}: {
  version: string
  loginRequired: boolean
  children: ReactNode
}) {
  const queryClient = useQueryClient()

  const logout = async () => {
    await api.logout()
    await queryClient.invalidateQueries()
  }

  return (
    <div className="flex min-h-full">
      <aside className="flex w-56 shrink-0 flex-col border-r border-slate-200 bg-white">
        <div className="border-b border-slate-100 px-4 py-4">
          <p className="text-sm font-semibold text-slate-900">AgoraModel</p>
          <p className="mt-0.5 text-xs text-slate-500">v{version}</p>
        </div>
        <nav className="flex flex-1 flex-col gap-1 p-3">
          {NAV_ITEMS.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              className={({ isActive }) =>
                cn(
                  'flex items-center gap-2 rounded-md px-3 py-2 text-sm transition-colors',
                  isActive ? 'bg-slate-900 text-white' : 'text-slate-700 hover:bg-slate-100',
                )
              }
            >
              <item.icon className="h-4 w-4" />
              {item.label}
            </NavLink>
          ))}
        </nav>
        {loginRequired ? (
          <div className="border-t border-slate-100 p-3">
            <Button variant="ghost" size="sm" className="w-full justify-start" onClick={logout}>
              <LogOut className="h-4 w-4" />
              退出登录
            </Button>
          </div>
        ) : null}
      </aside>

      <main className="min-w-0 flex-1 px-6 py-6">{children}</main>
    </div>
  )
}
