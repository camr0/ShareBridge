<template>
    <div class="sb-personal-settings">
        <div v-if="settings.loading" class="sb-settings-loading">
            Loading…
        </div>
        <template v-else>
            <div class="sb-field">
                <label for="sb-agent-url">Agent URL</label>
                <input
                    id="sb-agent-url"
                    data-testid="agent-url-input"
                    type="text"
                    :value="settings.agentUrl"
                    placeholder="http://localhost:7878"
                    class="sb-input"
                    @input="settings.agentUrl = ($event.target as HTMLInputElement).value"
                />
            </div>
            <div class="sb-field">
                <label for="sb-api-key">API Key</label>
                <input
                    id="sb-api-key"
                    data-testid="api-key-input"
                    type="password"
                    :value="settings.apiKey"
                    placeholder="sb_agent_..."
                    class="sb-input"
                    @input="settings.apiKey = ($event.target as HTMLInputElement).value"
                />
            </div>
            <div class="sb-settings-actions">
                <button data-testid="save-btn" class="button-vue" :disabled="saving" @click="save">
                    {{ saving ? 'Saving…' : 'Save' }}
                </button>
                <span v-if="saved" class="sb-saved-msg">Saved</span>
            </div>
            <p class="sb-settings-hint">
                Find the Agent URL and API key in your ShareBridge agent settings page.
            </p>
        </template>
    </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'

import { useSettingsStore } from '../stores/settings'

const settings = useSettingsStore()
const saving   = ref(false)
const saved    = ref(false)

const save = async () => {
    saving.value = true
    saved.value  = false
    try {
        await settings.saveSettings()
        saved.value = true
        setTimeout(() => { saved.value = false }, 3000)
    } finally {
        saving.value = false
    }
}

onMounted(() => settings.fetchSettings())
</script>