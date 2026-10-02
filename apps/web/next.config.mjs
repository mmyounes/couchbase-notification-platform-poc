/** @type {import('next').NextConfig} */
export default {
  // Standalone output keeps the runtime image small: Next traces only the
  // files actually reachable at runtime instead of shipping node_modules.
  output: 'standalone',
  reactStrictMode: true,
  transpilePackages: ['@ncgr/shared'],
  eslint: { ignoreDuringBuilds: true },

  // '/' is not a page: the first nav tile is the notification dashboard, so the
  // bare origin sends you there. 307 rather than 308 on purpose - a permanent
  // redirect is cached hard by browsers and would outlive any future change to
  // the landing page. Unlike rewrites(), this bakes in no upstream host.
  async redirects () {
    return [{ source: '/', destination: '/notifications', permanent: false }]
  },

  // No rewrites(): the API proxy is a runtime route handler at
  // src/app/api/[...path]/route.ts. Rewrites bake the upstream host into the
  // build, which broke when the topology changed underneath an existing image.
}
