import { createApp, defineCustomElement, h, ref } from 'vue'
import { createPinia } from 'pinia'
import { getSidebar } from '@nextcloud/files'
import type { ISidebarTab } from '@nextcloud/files'
import { t } from '@nextcloud/l10n'
import ShareBridgeTab from './components/ShareBridgeTab.vue'
import PersonalSettings from './components/PersonalSettings.vue'

// Shared pinia instance. setActivePinia makes it available to stores accessed
// inside defineCustomElement-created apps (which have their own Vue app context).
const pinia = createPinia()

// ─── Personal Settings ───────────────────────────────────────────────────────
// PersonalSection.php renders <div id="sharebridge-personal-settings"> in the
// NC Personal Settings page. Mount the Vue component there when that page is active.
document.addEventListener('DOMContentLoaded', () => {
    const el = document.getElementById('sharebridge-personal-settings')
    if (el) {
        createApp(PersonalSettings).use(pinia).mount(el)
    }
})

// ─── Sidebar Tab ─────────────────────────────────────────────────────────────
// Hybrid registration: NC 26-32 exposes window.OCA.Files.Sidebar (legacy global).
// NC 33+ removed that global and uses getSidebar() from @nextcloud/files v4.
// We bundle @nextcloud/files v4; on NC 26-32 we skip getSidebar() entirely and
// fall back to the OCA.Files.Sidebar global that NC provides natively.

const ICON_SVG = '<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="currentColor"><path d="M18 16.08c-.76 0-1.44.3-1.96.77L8.91 12.7c.05-.23.09-.46.09-.7s-.04-.47-.09-.7l7.05-4.11c.54.5 1.25.81 2.04.81 1.66 0 3-1.34 3-3s-1.34-3-3-3-3 1.34-3 3c0 .24.04.47.09.7L8.04 9.81C7.5 9.31 6.79 9 6 9c-1.66 0-3 1.34-3 3s1.34 3 3 3c.79 0 1.5-.31 2.04-.81l7.12 4.16c-.05.21-.08.43-.08.65 0 1.61 1.31 2.92 2.92 2.92s2.92-1.31 2.92-2.92-1.31-2.92-2.92-2.92z"/></svg>'

// TypeScript type for the NC 26-32 global (not in any @types package).
declare global {
    interface Window {
        OCA?: {
            Files?: {
                Sidebar?: {
                    registerTab: (tab: unknown) => void
                    Tab: new (options: {
                        id: string
                        name: string
                        iconSvgInline?: string
                        mount: (el: HTMLElement, fileInfo: { id: number; path: string }) => void
                        update: (fileInfo: { id: number; path: string }) => void
                        destroy: () => void
                    }) => unknown
                }
            }
        }
    }
}

if (window.OCA?.Files?.Sidebar) {
    // ── NC 26-32: OCA.Files.Sidebar legacy global ─────────────────────────
    // fileInfo.id is the numeric fileid; fileInfo.path is the NC path.
    let app: ReturnType<typeof createApp> | null = null
    const currentNode = ref({ id: '', path: '' })

    window.OCA.Files.Sidebar.registerTab(
        new window.OCA.Files.Sidebar.Tab({
            id:           'sharebridge',
            name:         t('sharebridge', 'ShareBridge'),
            iconSvgInline: ICON_SVG,
            mount(el: HTMLElement, fileInfo: { id: number; path: string }) {
                // Convert legacy numeric id to string to match INode.id contract
                currentNode.value = { id: String(fileInfo.id), path: fileInfo.path }
                app = createApp({ render: () => h(ShareBridgeTab, { node: currentNode.value }) })
                app.use(pinia).mount(el)
            },
            update(fileInfo: { id: number; path: string }) {
                currentNode.value = { id: String(fileInfo.id), path: fileInfo.path }
            },
            destroy() {
                app?.unmount()
                app = null
            },
        })
    )
} else {
    // ── NC 33+: @nextcloud/files v4 getSidebar() + web component ──────────
    // defineCustomElement wraps ShareBridgeTab as a custom element.
    // shadowRoot: false allows NC global CSS (theming) to reach the component.
    const tab: ISidebarTab = {
        id:            'sharebridge',
        displayName:   t('sharebridge', 'ShareBridge'),
        iconSvgInline: ICON_SVG,
        order:         50,
        tagName:       'sharebridge-files-sidebar-tab' as `${string}-${string}`,
        enabled() { return true },
        async onInit() {
            const SidebarTabEl = defineCustomElement(ShareBridgeTab, {
                shadowRoot: false,
                configureApp(app) { app.use(pinia) },
            })
            customElements.define('sharebridge-files-sidebar-tab', SidebarTabEl)
        },
    }
    getSidebar().registerTab(tab)
}