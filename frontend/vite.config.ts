import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { VitePWA } from 'vite-plugin-pwa'

// https://vite.dev/config/
export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    VitePWA({
      // injectManifest, not generateSW: the caching rules here are the whole
      // risk of this feature (a stale balance shown as current is worse than
      // an error), so they live in reviewable source at src/sw.ts rather than
      // in plugin config that generates code nobody reads.
      strategies: 'injectManifest',
      srcDir: 'src',
      filename: 'sw.ts',

      // 'prompt', not 'autoUpdate'. autoUpdate reloads the page out from under
      // whoever is mid-way through editing a budget; the user gets a bar and
      // chooses the moment.
      registerType: 'prompt',
      injectRegister: null, // registration is explicit, in PwaPrompts.tsx

      // index.html already carries the icon and theme-colour tags, so the
      // plugin only needs to inject the manifest link.
      includeAssets: [],

      // The plugin precaches every manifest icon by default, on top of whatever
      // the glob picks up. The OS fetches those from the manifest itself at
      // install time and nothing in the app renders them, so precaching them is
      // a quarter of the payload spent twice. See globIgnores below.
      includeManifestIcons: false,

      manifest: {
        name: 'Ledgermancy',
        short_name: 'Ledgermancy',
        description: "A self-hosted hub for your household's finances.",
        // Both from BRAND.md (ink-950). The app is dark-only, so the splash
        // and the status bar match the page rather than flashing white.
        theme_color: '#07060d',
        background_color: '#07060d',
        display: 'standalone',
        start_url: '/',
        scope: '/',
        id: '/',
        orientation: 'portrait-primary',
        icons: [
          {
            src: '/android-chrome-192x192.png',
            sizes: '192x192',
            type: 'image/png',
            purpose: 'any',
          },
          {
            src: '/android-chrome-512x512.png',
            sizes: '512x512',
            type: 'image/png',
            purpose: 'any',
          },
          // Without this Android masks the transparent "any" icon straight
          // through the artwork and the install looks broken. It carries its
          // own opaque ground and keeps the mark inside the 80% safe circle.
          {
            src: '/maskable-512x512.png',
            sizes: '512x512',
            type: 'image/png',
            purpose: 'maskable',
          },
        ],
      },

      injectManifest: {
        globPatterns: ['**/*.{js,css,html,ico,png,svg,woff,woff2}'],
        // The precache list is the app shell only — nothing here is user data.
        //
        // The install-time icons are excluded on purpose: they are a quarter of
        // the payload, the OS fetches and keeps them from the manifest itself,
        // and nothing in the app ever renders them. `logo.png` stays, because
        // Brand.tsx does render it.
        globIgnores: [
          '**/node_modules/**/*',
          'sw.js',
          'workbox-*.js',
          'android-chrome-512x512.png',
          'maskable-512x512.png',
        ],
      },

      // A service worker in `vite dev` caches modules the dev server is trying
      // to hot-replace, which produces bugs that do not exist in a real build.
      // Test the PWA with `npm run build && npm run preview`.
      devOptions: { enabled: false },
    }),
  ],
  server: {
    port: 5173,
    // Proxy the API through the dev server so the browser sees a single
    // origin. That keeps the session cookie same-origin (SameSite=Strict is
    // honoured without special-casing) and sidesteps CORS entirely in dev.
    proxy: {
      '/api': { target: 'http://localhost:8080', changeOrigin: false },
      '/healthz': { target: 'http://localhost:8080', changeOrigin: false },
    },
  },
})
