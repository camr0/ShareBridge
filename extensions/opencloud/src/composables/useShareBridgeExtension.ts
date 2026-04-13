import { computed } from 'vue'
import type { SidebarPanelExtension, SideBarPanelContext } from '@opencloud-eu/web-pkg'
import type { Item } from '@opencloud-eu/web-client'
import ShareBridgePanel from '../components/ShareBridgePanel.vue'

export const useShareBridgeExtension = () => {
  const extension = computed<SidebarPanelExtension<Item, Item, Item>>(() => ({
    id: 'com.sharebridge.sidebar-panel',
    type: 'sidebarPanel',
    extensionPointIds: ['global.files.sidebar'],
    panel: {
      name: 'sharebridge',
      icon: 'share',
      title: () => 'ShareBridge',
      component: ShareBridgePanel,
      componentAttrs: (context: SideBarPanelContext<Item, Item, Item>) => ({
        resource: context.items?.[0],
      }),
      isRoot: () => true,
      isVisible: (context: SideBarPanelContext<Item, Item, Item>) =>
        (context.items?.length ?? 0) === 1,
    },
  }))

  return { extension }
}