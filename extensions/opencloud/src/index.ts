import { computed } from 'vue'
import 'vue3-gettext' // ensure AMD loader provides host's initialized gettext instance
import { defineWebApplication } from '@opencloud-eu/web-pkg'
import { useShareBridgeExtension } from './composables/useShareBridgeExtension'

// Inject stylesheet (AMD builds don't auto-inject CSS)
const link = document.createElement('link')
link.rel = 'stylesheet'
link.href = '/assets/apps/web-app-sharebridge/style.css'
document.head.appendChild(link)

export default defineWebApplication({
  setup() {
    const { extension } = useShareBridgeExtension()

    return {
      appInfo: {
        name: 'ShareBridge',
        id: 'web-app-sharebridge',
      },
      translations: {},
      extensions: computed(() => [extension.value]),
    }
  },
})