import type { Config } from 'tailwindcss'
export default {
  content: ['./src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        bg:      '#0b0e14',
        panel:   '#141922',
        border:  '#232a36',
        muted:   '#8b96a8',
        text:    '#e6ebf2',
        accent:  '#4c8dff',
        ok:      '#2ecc71',
        warn:    '#f0a020',
        danger:  '#e5484d',
        pending: '#8b96a8',
      },
      fontFamily: {
        mono: ['ui-monospace', 'SFMono-Regular', 'Menlo', 'monospace'],
      },
    },
  },
  plugins: [],
} satisfies Config
