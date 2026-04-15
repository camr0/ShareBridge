import { defineStore } from 'pinia'
import axios from '@nextcloud/axios'
import { generateUrl } from '@nextcloud/router'

export const useSettingsStore = defineStore('sharebridge-settings', {
    state: () => ({
        agentUrl: '' as string,
        apiKey:   '' as string,
        loading:  false,
        loaded:   false,
    }),
    getters: {
        isConfigured: (state) =>
            state.loaded && !state.loading && state.agentUrl !== '' && state.apiKey !== '',
    },
    actions: {
        async fetchSettings(): Promise<void> {
            if (this.loaded) return
            this.loading = true
            try {
                const response = await axios.get(generateUrl('/apps/sharebridge/api/settings'))
                this.agentUrl = response.data.agent_url ?? ''
                this.apiKey   = response.data.api_key   ?? ''
            } catch {
                // treat as unconfigured — show "Configure in Personal Settings" prompt
            } finally {
                this.loading = false
                this.loaded  = true
            }
        },
        async saveSettings(): Promise<void> {
            await axios.put(generateUrl('/apps/sharebridge/api/settings'), {
                agent_url: this.agentUrl,
                api_key:   this.apiKey,
            })
        },
    },
})