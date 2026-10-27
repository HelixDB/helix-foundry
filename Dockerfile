# Helix Foundry's site as a local build (src/shared/deployment.ts): one owner
# on this machine, no sign-in. It needs, at run time:
#   HELIX_WFE_ORIGIN     the local API, e.g. http://pod:8081
#   FOUNDRY_ORIGIN       the loopback origin the browser uses, e.g. http://localhost:3000
#   FOUNDRY_LOCAL_TOKEN  the token the local API accepts as its owner, or
#   FOUNDRY_LOCAL_TOKEN_FILE  a file holding it, read until it has one
# The hosted site is built by Vercel, not from this file.

FROM node:24-bookworm-slim AS build
WORKDIR /app
RUN corepack enable
COPY package.json pnpm-lock.yaml ./
# The install's postinstall copies pdf.js's assets into public/pdfjs.
COPY scripts/pdfjs-assets.mjs scripts/
RUN pnpm install --frozen-lockfile
COPY . .
ENV NEXT_PUBLIC_FOUNDRY_MODE=local NEXT_TELEMETRY_DISABLED=1
RUN pnpm build \
    && cp -r public .next/standalone/ \
    && cp -r .next/static .next/standalone/.next/

FROM node:24-bookworm-slim
WORKDIR /app
ENV NODE_ENV=production NEXT_TELEMETRY_DISABLED=1 PORT=3000 HOSTNAME=0.0.0.0
COPY --from=build --chown=node:node /app/.next/standalone ./
USER node
EXPOSE 3000
HEALTHCHECK --interval=10s --timeout=5s CMD ["node", "-e", "fetch('http://127.0.0.1:3000/').then(r => process.exit(r.ok ? 0 : 1), () => process.exit(1))"]
CMD ["node", "server.js"]
