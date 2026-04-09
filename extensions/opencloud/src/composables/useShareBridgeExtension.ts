import { computed } from 'vue'
import { useGettext } from '@opencloud-eu/web-pkg'
import type { SidebarPanelExtension } from '@opencloud-eu/web-pkg'
import ShareBridgePanel from '../components/ShareBridgePanel.vue'

export const useShareBridgeExtension = () => {
  const { $gettext } = useGettext()

  const extension = computed<SidebarPanelExtension<unknown>>(() => ({
    id: 'com.sharebridge.sidebar-panel',
    type: 'sidebarPanel',
    extensionPointIds: ['global.files.sidebar'],
    panel: {
      name: 'sharebridge',
      icon: 'share',
      title: () => $gettext('ShareBridge'),
      component: ShareBridgePanel,
      isRoot: () => true,
      // Note: verify field name against your SDK version.
      // May be 'resources' instead of 'items' in some versions.
      isVisible: ({ items }: { items?: unknown[] }) => (items?.length ?? 0) === 1,
    },
  }))

  return { extension }
}