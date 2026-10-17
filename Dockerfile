FROM node:24-bookworm-slim AS build
WORKDIR /app
RUN corepack enable && corepack prepare pnpm@10.7.0 --activate
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY packages ./packages
RUN pnpm install --frozen-lockfile
COPY . .
RUN pnpm --filter @helix-foundry/shared build
RUN pnpm --filter @helix-foundry/sdk build
RUN npx tsc --noEmit
RUN npx vite build apps/web
FROM node:24-bookworm-slim
WORKDIR /app
RUN corepack enable && corepack prepare pnpm@10.7.0 --activate
COPY --from=build --chown=node:node /app /app
RUN mkdir -p /data && chown node:node /data
USER node
ENV NODE_ENV=production HOST=0.0.0.0 PORT=3001 DATA_DIR=/data
EXPOSE 3001
CMD ["node","--import","tsx","apps/api/src/server.ts"]
