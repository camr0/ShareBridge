<template>
	<NcSettingsSection
		name="ShareBridge"
		description="Connect to your local ShareBridge agent to share files via WebRTC."
	>
		<NcLoadingIcon v-if="settings.loading" :size="32" />

		<template v-else>
			<NcTextField
				data-testid="agent-url-input"
				label="Agent URL"
				:value="settings.agentUrl"
				placeholder="http://localhost:7878"
				@update:value="settings.agentUrl = $event"
			/>

			<NcPasswordField
				data-testid="api-key-input"
				label="API Key"
				:value="settings.apiKey"
				placeholder="sb_agent_..."
				@update:value="settings.apiKey = $event"
			/>

			<div class="sb-settings-actions">
				<NcButton data-testid="save-btn" type="primary" :disabled="saving" @click="save">
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
import { NcSettingsSection, NcTextField, NcPasswordField, NcButton, NcLoadingIcon } from '@nextcloud/vue'
import { useSettingsStore } from '../stores/settings'

const settings = useSettingsStore()
const saving = ref(false)
const saved = ref(false)

const save = async () => {
	saving.value = true
	saved.value = false
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

<style scoped>
.sb-settings-actions {
	display: flex;
	align-items: center;
	gap: 12px;
	margin-top: 8px;
}

.sb-saved-msg {
	color: var(--color-success);
	font-size: 13px;
}

.sb-settings-hint {
	margin-top: 8px;
	font-size: 13px;
	color: var(--color-text-maxcontrast);
}
</style>