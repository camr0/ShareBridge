<template>
	<div class="sb-tab">
		<NcLoadingIcon v-if="settings.loading" :size="32" />

		<NcEmptyContent
			v-else-if="!settings.isConfigured"
			data-testid="configure-prompt"
			name="ShareBridge not configured"
			description="Configure ShareBridge in your Personal Settings to get started."
		/>

		<template v-else>
			<NcNoteCard v-if="error" data-testid="error-msg" type="error">
				{{ error }}
			</NcNoteCard>

			<NcLoadingIcon v-else-if="loadingShares" :size="32" />

			<template v-else>
				<NcEmptyContent
					v-if="shares.length === 0"
					data-testid="empty-state"
					name="No shares"
					description="No ShareBridge shares for this file."
				/>

				<div v-else data-testid="share-list" class="sb-share-list">
					<ShareCard
						v-for="share in shares"
						:key="share.code"
						:share="share"
						data-testid="share-card"
						@revoke="handleRevoke"
					/>
				</div>

				<NcButton data-testid="create-share-btn" type="primary" @click="showModal = true">
					Create ShareBridge Share
				</NcButton>
			</template>

			<CreateShareModal
				v-if="showModal"
				:file-path="props.node.path"
				:turn-available="turnAvailable"
				:default-expiry-hours="defaultExpiryHours"
				:default-max-downloads="defaultMaxDownloads"
				:default-relay-only="defaultRelayOnly"
				@close="showModal = false"
				@created="handleCreated"
			/>
		</template>
	</div>
</template>

<script setup lang="ts">
import { ref, watch, onMounted } from 'vue'
import { NcLoadingIcon, NcEmptyContent, NcNoteCard, NcButton } from '@nextcloud/vue'
import { useSettingsStore } from '../stores/settings'
import { useAgentClient } from '../composables/useAgentClient'
import { useNextcloudOCS } from '../composables/useNextcloudOCS'
import ShareCard from './ShareCard.vue'
import CreateShareModal from './CreateShareModal.vue'
import type { Share, CreateShareResult } from '../types'

const props = defineProps<{
	node: { id?: string; path: string }
}>()

const settings = useSettingsStore()
const { listShares, revokeShare, getSettings } = useAgentClient()
const { getNcShareId, deleteOCSShare, deleteNcShareId } = useNextcloudOCS()

const shares = ref<Share[]>([])
const loadingShares = ref(false)
const error = ref('')
const showModal = ref(false)
const turnAvailable = ref(false)
const defaultExpiryHours = ref(24)
const defaultMaxDownloads = ref(0)
const defaultRelayOnly = ref(false)

const loadShares = async () => {
	loadingShares.value = true
	error.value = ''
	try {
		shares.value = await listShares(props.node.id!)
	} catch {
		error.value = "Cannot connect to ShareBridge agent. Check the agent URL and ensure it's running."
	} finally {
		loadingShares.value = false
	}
}

const applyAgentSettings = async () => {
	try {
		const s = await getSettings()
		turnAvailable.value = s.turn_available
		defaultExpiryHours.value = s.default_expiry_hours
		defaultMaxDownloads.value = s.default_max_downloads
		defaultRelayOnly.value = s.default_relay_only
	} catch {
		// agent unreachable — form defaults stay as-is
	}
}

const handleRevoke = async (code: string) => {
	error.value = ''
	try {
		const ncShareId = await getNcShareId(code)
		await Promise.all([revokeShare(code), deleteOCSShare(ncShareId), deleteNcShareId(code)])
	} catch {
		error.value = 'Failed to fully revoke share. The Nextcloud link may still be accessible.'
	}
	if (!error.value) {
		await loadShares()
	}
}

const handleCreated = async (_result: CreateShareResult) => {
	showModal.value = false
	await loadShares()
}

watch(
	[() => props.node?.id, () => settings.isConfigured],
	([nodeId, isConfigured]) => {
		if (nodeId && isConfigured) {
			Promise.all([loadShares(), applyAgentSettings()])
		}
	},
	{ immediate: true }
)

onMounted(() => settings.fetchSettings())
</script>

<style scoped>
.sb-tab {
	padding: 8px 0;
}

.sb-share-list {
	display: flex;
	flex-direction: column;
	gap: 8px;
	margin-bottom: 12px;
}
</style>