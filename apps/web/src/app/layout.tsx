import type { Metadata } from 'next'
import './globals.css'
import { Providers } from './providers'
import { Nav } from '@/components/Nav'

export const metadata: Metadata = {
  title: 'Notification Platform — Couchbase PoC',
  description: 'Event processing, delivery tracking and search over 500M notifications',
}

export default function RootLayout ({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body className="min-h-screen">
        <Providers>
          <Nav />
          <main className="mx-auto max-w-[1600px] px-6 py-6">{children}</main>
        </Providers>
      </body>
    </html>
  )
}
