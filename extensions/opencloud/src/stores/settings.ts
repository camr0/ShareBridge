import { defineStore } from 'pinia'

export const useSettingsStore = defineStore('sharebridge-settings', {
  state: () => ({
    agentUrl: localStorage.getItem('sharebridge_agent_url') ?? '',
    apiKey: localStorage.getItem('sharebridge_api_key') ?? '',
  }),
  getters: {
    isConfigured: (state) => state.agentUrl !== '' && state.apiKey !== '',
  },
  actions: {
    setAgentUrl(url: string) {
      this.agentUrl = url
      localStorage.setItem('sharebridge_agent_url', url)
    },
    setApiKey(key: string) {
      this.apiKey = key
      localStorage.setItem('sharebridge_api_key', key)
    },
  },
})