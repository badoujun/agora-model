import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'node:path'

// 构建产物直接落到 Go 包内，便于 //go:embed 内嵌成单文件（T4.9）
const OUT_DIR = path.resolve(__dirname, '../internal/webui/dist')

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  build: {
    outDir: OUT_DIR,
    emptyOutDir: true,
    chunkSizeWarningLimit: 1200,
  },
  server: {
    port: 5173,
    // 开发期把 /api 代理到本地网关（生产由后端直接托管静态文件）
    proxy: {
      '/api': {
        target: process.env.AGORA_API_TARGET ?? 'http://127.0.0.1:9090',
        changeOrigin: true,
      },
    },
  },
})
