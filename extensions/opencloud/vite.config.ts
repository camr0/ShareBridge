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
      external: ['vue', '@opencloud-eu/web-pkg', 'pinia', 'vue3-gettext'],
    },
  },
  test: {
    environment: 'happy-dom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
  },
})