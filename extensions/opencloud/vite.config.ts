import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  build: {
    lib: {
      entry: 'src/index.ts',
      formats: ['amd'],
      name: 'web-app-sharebridge',
      fileName: () => 'web-app-sharebridge.js',
    },
    rollupOptions: {
      external: ['vue', '@opencloud-eu/web-pkg', 'pinia'],
    },
  },
  test: {
    environment: 'happy-dom',
    globals: true,
  },
})