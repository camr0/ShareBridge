import { ref } from 'vue'
import { defineWebApplication } from '@opencloud-eu/web-pkg'

export default defineWebApplication({
  setup() {
    return {
      appInfo: {
        name: 'ShareBridge',
        id: 'web-app-sharebridge',
      },
      extensions: ref([]),
    }
  },
})