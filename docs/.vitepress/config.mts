import { defineConfig } from 'vitepress'

const base = '/smartclass-dispatchub/'

export default defineConfig({
  title: 'SmartClass Dispatch Hub',
  description: 'Timetable-driven recording scheduler and control plane for SmartClass',
  base,
  cleanUrls: true,
  lastUpdated: true,
  themeConfig: {
    nav: [
      { text: 'Guide', link: '/guide/getting-started' },
      { text: 'API Reference', link: '/api/overview' },
      { text: 'Deployment', link: '/guide/deploy' },
      {
        text: 'GitHub',
        link: 'https://github.com/crazy4chicken/smartclass-dispatchub'
      }
    ],
    sidebar: {
      '/guide/': [
        {
          text: 'Guide',
          items: [
            { text: 'Getting Started', link: '/guide/getting-started' },
            { text: 'Deployment', link: '/guide/deploy' },
            { text: 'Permissions', link: '/guide/permissions' },
            { text: 'API Usage', link: '/guide/api-usage' },
            { text: 'Operations', link: '/guide/operations' }
          ]
        }
      ],
      '/api/': [
        {
          text: 'API Reference',
          items: [
            { text: 'Overview', link: '/api/overview' },
            { text: 'Terms', link: '/api/reference/terms' },
            { text: 'Rooms', link: '/api/reference/rooms' },
            { text: 'Timetable', link: '/api/reference/timetable' },
            { text: 'Sessions', link: '/api/reference/sessions' },
            { text: 'Recording', link: '/api/reference/recording' },
            { text: 'Health', link: '/api/reference/health' },
            // Static file, not a route: VitePress leaves its URL untouched,
            // so the deployment base has to be part of the link.
            { text: 'OpenAPI document', link: `${base}openapi.yaml` }
          ]
        }
      ]
    },
    search: {
      provider: 'local'
    }
  }
})
