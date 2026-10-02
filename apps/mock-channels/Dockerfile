FROM node:22-bookworm-slim AS builder
WORKDIR /repo
RUN corepack enable && corepack prepare pnpm@9.12.0 --activate

COPY pnpm-workspace.yaml package.json tsconfig.base.json ./
COPY packages/shared/package.json       packages/shared/
COPY apps/mock-channels/package.json    apps/mock-channels/
RUN pnpm install --frozen-lockfile=false \
      --filter @ncgr/mock-channels... --filter @ncgr/mock-channels

COPY packages/shared/ packages/shared/
COPY apps/mock-channels/ apps/mock-channels/
RUN pnpm --filter @ncgr/shared build && pnpm --filter @ncgr/mock-channels build

FROM node:22-bookworm-slim AS runtime
WORKDIR /repo
ENV NODE_ENV=production
RUN groupadd -r ncgr && useradd -r -g ncgr ncgr

COPY --from=builder /repo/node_modules                       ./node_modules
COPY --from=builder /repo/packages/shared/dist               ./packages/shared/dist
COPY --from=builder /repo/packages/shared/package.json       ./packages/shared/
COPY --from=builder /repo/apps/mock-channels/dist            ./apps/mock-channels/dist
COPY --from=builder /repo/apps/mock-channels/package.json    ./apps/mock-channels/
COPY --from=builder /repo/apps/mock-channels/node_modules    ./apps/mock-channels/node_modules

USER ncgr
WORKDIR /repo/apps/mock-channels
EXPOSE 8081
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
  CMD node -e "fetch('http://127.0.0.1:8081/health').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"
CMD ["node", "dist/server.js"]
