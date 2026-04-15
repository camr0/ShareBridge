import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import path from 'path'

export default defineConfig({
    plugins: [vue()],
    resolve: {
        alias: {
            '@nextcloud/axios':  path.resolve(__dirname, 'src/test/mocks/nextcloud-axios.ts'),
            '@nextcloud/router': path.resolve(__dirname, 'src/test/mocks/nextcloud-router.ts'),
            '@nextcloud/l10n':   path.resolve(__dirname, 'src/test/mocks/nextcloud-l10n.ts'),
            '@nextcloud/vue':    path.resolve(__dirname, 'src/test/mocks/nextcloud-vue.ts'),
            '@nextcloud/files':  path.resolve(__dirname, 'src/test/mocks/nextcloud-files.ts'),
        },
    },
    test: {
        environment: 'happy-dom',
        globals: true,
        setupFiles: ['./src/test/setup.ts'],
    },
})