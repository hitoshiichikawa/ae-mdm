# ae-mdm tenant-console SPA 用 multi-stage Dockerfile
# requirements.md Requirement 6.3, 6.4 / NFR 4.1
#
# Build context は repository root（docker-compose.yml の build.context 参照）。
# workspace 構造のため shared / tenant-console / admin-console の package.json を全て
# 取り込んでから npm install する。

# ---- Build stage ----
FROM node:20-alpine AS builder
WORKDIR /src

# workspace ルート + 3 パッケージの manifest を先にコピー（依存解決キャッシュ）
COPY package.json package-lock.json ./
COPY frontend/shared/package.json frontend/shared/package.json
COPY frontend/tenant-console/package.json frontend/tenant-console/package.json
COPY frontend/admin-console/package.json frontend/admin-console/package.json

RUN npm ci --no-audit --no-fund

# shared と tenant-console のソースのみコピー
COPY frontend/shared frontend/shared
COPY frontend/tenant-console frontend/tenant-console

# tenant-console を build（shared は tsc --noEmit のみで output が無いので type 解決のみ用途）
RUN npm run build --workspace=@ae-mdm/tenant-console

# ---- Final stage (nginx) ----
FROM nginx:1.27-alpine AS final
COPY --from=builder /src/frontend/tenant-console/dist /usr/share/nginx/html
COPY frontend/tenant-console/nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 80
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -q --spider http://localhost/ || exit 1
