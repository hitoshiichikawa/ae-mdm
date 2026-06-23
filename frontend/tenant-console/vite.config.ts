import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// tenant-console SPA は 5173 番ポートで dev サーバを起動し、`/api` を backend api サービスへ
// リバースプロキシする。production build は Docker 内で nginx が同等のリバースプロキシを行う
// （frontend/tenant-console/nginx.conf 参照）。
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: true,
  },
});
