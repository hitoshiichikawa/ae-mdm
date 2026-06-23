import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// admin-console SPA は 5174 番ポートで dev サーバを起動し、`/api/admin` を backend api サービスへ
// リバースプロキシする。production build は Docker 内で nginx が同等のリバースプロキシを行う
// （frontend/admin-console/nginx.conf 参照）。
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5174,
    proxy: {
      '/api/admin': {
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
