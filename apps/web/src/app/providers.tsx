'use client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useState } from 'react'

export function Providers ({ children }: { children: React.ReactNode }) {
  const [client] = useState(() => new QueryClient({
    defaultOptions: {
      queries: {
        // Polling drives the live views; retrying a failed poll just delays the
        // next good one and hides the error from the operator.
        retry: false,
        refetchOnWindowFocus: false,
        staleTime: 0,
      },
    },
  }))
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>
}
