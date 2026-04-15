// src/stores/settings.ts — temporary stub, replaced in Task 4
import { defineStore } from 'pinia'
export const useSettingsStore = defineStore('sharebridge-settings', {
	state: () => ({ agentUrl: '', apiKey: '', loading: false, loaded: false }),
	getters: { isConfigured: (s) => s.agentUrl !== '' && s.apiKey !== '' },
	actions: {
		async fetchSettings() {},
		async saveSettings()  {},
	},
})