# ae-mdm admin-console SPA 用 multi-stage Dockerfile
# requirements.md Requirement 6.3, 6.4 / NFR 4.1

# ---- Build stage ----
FROM node:20-alpine AS builder
WORKDIR /src

COPY package.json package-lock.json ./
COPY frontend/shared/package.json frontend/shared/package.json
COPY frontend/tenant-console/package.json frontend/tenant-console/package.json
COPY frontend/admin-console/package.json frontend/admin-console/package.json

RUN npm ci --no-audit --no-fund

COPY frontend/shared frontend/shared
COPY frontend/admin-console frontend/admin-console

RUN npm run build --workspace=@ae-mdm/admin-console

# ---- Final stage (nginx) ----
FROM nginx:1.27-alpine AS final
COPY --from=builder /src/frontend/admin-console/dist /usr/share/nginx/html
COPY frontend/admin-console/nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 80
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -q --spider http://localhost/ || exit 1
