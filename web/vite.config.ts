import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// 开发时 /api、/v1 代理到本地网关（先启动 gateway：cd gateway && go run ./cmd/gateway）
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/api': 'http://localhost:8080',
      '/v1': 'http://localhost:8080',
    },
  },
})
