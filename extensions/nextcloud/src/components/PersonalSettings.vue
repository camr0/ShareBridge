<template>
    <NcSettingsSection name="ShareBridge" description="Configure your ShareBridge agent connection.">
        <div v-if="settings.loading" class="sb-settings-loading">
            <NcLoadingIcon />
        </div>
        <template v-else>
            <NcTextField
                data-testid="agent-url-input"
                :value="settings.agentUrl"
                label="Agent URL"
                placeholder="http://localhost:7878"
                @update:model-value="settings.agentUrl = $event"
            />
            <NcTextField
                data-testid="api-key-input"
                :value="settings.apiKey"
                label="API Key"
                type="password"
                placeholder="sb_agent_..."
                @update:model-value="settings.apiKey = $event"
            />
            <div class="sb-settings-actions">
                <NcButton data-testid="save-btn" @click="save" :disabled="saving">
                    {{ saving ? 'Saving…' : 'Save' }}
                </NcButton>
                <span v-if="saved" class="sb-saved-msg">Saved</span>
            </div>
            <p class="sb-settings-hint">
                Find the Agent URL and API key in your ShareBridge agent settings page.
            </p>
        </template>
    </NcSettingsSection>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { NcSettingsSection, NcTextField, NcButton, NcLoadingIcon } from '@nextcloud/vue'
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