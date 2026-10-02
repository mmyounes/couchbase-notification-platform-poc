FROM node:22-bookworm-slim AS builder
WORKDIR /repo
RUN corepack enable && corepack prepare pnpm@9.12.0 --activate

COPY pnpm-workspace.yaml package.json tsconfig.base.json ./
COPY packages/shared/package.json packages/shared/
COPY apps/web/package.json        apps/web/
RUN pnpm install --frozen-lockfile=false --filter @ncgr/web... --filter @ncgr/web

COPY packages/shared/ packages/shared/
COPY apps/web/ apps/web/
RUN pnpm --filter @ncgr/shared build && pnpm --filter @ncgr/web build

FROM node:22-bookworm-slim AS runtime
WORKDIR /app
ENV NODE_ENV=production
RUN groupadd -r ncgr && useradd -r -g ncgr ncgr

# Next standalone output traces only the files reachable at runtime, so the
# image carries no node_modules tree of its own.
COPY --from=builder /repo/apps/web/.next/standalone ./
COPY --from=builder /repo/apps/web/.next/static     ./apps/web/.next/static
COPY --from=builder /repo/apps/web/public           ./apps/web/public

USER ncgr
EXPOSE 3000
ENV PORT=3000 HOSTNAME=0.0.0.0
CMD ["node", "apps/web/server.js"]
