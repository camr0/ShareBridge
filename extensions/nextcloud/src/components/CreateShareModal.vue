<template>
	<NcDialog name="Create ShareBridge Share" size="small" @closing="emit('close')">
		<div class="sb-modal-content">
			<NcSelect
				data-testid="expiry-select"
				:options="expiryOptions"
				:model-value="selectedExpiryOption"
				:clearable="false"
				label="label"
				:reduce="(opt: ExpiryOption) => opt.value"
				@update:model-value="form.expiryHours = Number($event)"
			/>

			<NcPasswordField
				data-testid="password-input"
				label="Password (optional)"
				:value="form.password"
				@update:value="form.password = $event"
			/>

			<NcTextField
				data-testid="max-downloads-input"
				label="Max Downloads (0 = unlimited)"
				type="number"
				:value="String(form.maxDownloads)"
				@update:value="form.maxDownloads = Number($event)"
			/>

			<NcCheckboxRadioSwitch
				data-testid="relay-only-input"
				v-model:checked="form.relayOnly"
			>
				Relay mode only
			</NcCheckboxRadioSwitch>

			<NcNoteCard v-if="form.relayOnly && !turnAvailable" data-testid="turn-warning" type="warning">
				Relay mode requires a TURN server. Shares may not connect without one.
			</NcNoteCard>

			<NcNoteCard v-if="error" data-testid="error-msg" type="error">
				{{ error }}
			</NcNoteCard>
		</div>
		<template #actions>
			<NcButton data-testid="cancel-btn" :disabled="loading" @click="emit('close')">
				Cancel
			</NcButton>
			<NcButton data-testid="create-btn" type="primary" :disabled="loading" @click="submit">
				{{ loading ? 'Creating…' : 'Create Share' }}
			</NcButton>
		</template>
	</NcDialog>
</template>

<script setup lang="ts">
import { ref, reactive, computed } from 'vue'
import { NcDialog, NcSelect, NcPasswordField, NcTextField, NcCheckboxRadioSwitch, NcNoteCard, NcButton } from '@nextcloud/vue'
import { useNextcloudOCS } from '../composables/useNextcloudOCS'
import { useAgentClient } from '../composables/useAgentClient'
import type { CreateShareResult } from '../types'

const PRESET_EXPIRY_HOURS = [1, 6, 12, 24, 72, 168, 720]

interface ExpiryOption {
	value: number
	label: string
}

const props = defineProps<{
	filePath: string
	turnAvailable?: boolean
	defaultExpiryHours?: number
	defaultMaxDownloads?: number
	defaultRelayOnly?: boolean
}>()
const emit = defineEmits<{
	close: []
	created: [result: CreateShareResult]
}>()

const { createOCSShare, saveNcShareId } = useNextcloudOCS()
const { createShare } = useAgentClient()

const loading = ref(false)
const error = ref('')
const form = reactive({
	expiryHours: props.defaultExpiryHours ?? 24,
	password: '',
	maxDownloads: props.defaultMaxDownloads ?? 0,
	relayOnly: props.defaultRelayOnly ?? false,
})

const expiryOptions = computed(() => {
	const opts = PRESET_EXPIRY_HOURS.map(h => ({ value: h, label: formatExpiry(h) }))
	if (props.defaultExpiryHours && !PRESET_EXPIRY_HOURS.includes(props.defaultExpiryHours)) {
		opts.push({ value: props.defaultExpiryHours, label: `${props.defaultExpiryHours} hours` })
	}
	return opts.sort((a, b) => a.value - b.value)
})

const selectedExpiryOption = computed(() => {
	return expiryOptions.value.find(opt => opt.value === form.expiryHours) ?? expiryOptions.value[0]
})

const formatExpiry = (hours: number): string => {
	if (hours < 24) return hours === 1 ? '1 hour' : `${hours} hours`
	const days = hours / 24
	return days === 1 ? '1 day' : `${days} days`
}

const submit = async () => {
	loading.value = true
	error.value = ''
	try {
		const { shareUrl, shareId } = await createOCSShare(props.filePath, form.expiryHours)
		const result = await createShare({
			share_url: shareUrl,
			password: form.password || undefined,
			expiry_hours: form.expiryHours,
			max_downloads: form.maxDownloads,
			relay_only: form.relayOnly,
		})
		await saveNcShareId(result.code, shareId)
		emit('created', result)
	} catch (err: unknown) {
		const message = err instanceof Error ? err.message : ''
		if (message === 'PASSWORD_REQUIRED') {
			error.value = 'Your Nextcloud requires passwords on public links. Please contact your admin.'
		} else if (message === 'OCS_SHARE_FAILED') {
			error.value = 'Failed to create Nextcloud share. Please try again.'
		} else {
			error.value = 'Failed to create ShareBridge share. Please try again.'
		}
	} finally {
		loading.value = false
	}
}
</script>

<style scoped>
.sb-modal-content {
	display: flex;
	flex-direction: column;
	gap: 16px;
}
</style>