import { computed } from 'vue'
import 'vue3-gettext' // ensure AMD loader provides host's initialized gettext instance
import { defineWebApplication } from '@opencloud-eu/web-pkg'
import { useShareBridgeExtension } from './composables/useShareBridgeExtension'

export default defineWebApplication({
  setup() {
    const { extension } = useShareBridgeExtension()

    return {
      appInfo: {
        name: 'ShareBridge',
        id: 'web-app-sharebridge',
      },
      extensions: computed(() => [extension.value]),
    }
  },
})