<template>
    <NcModal name="Create ShareBridge Share" @close="emit('close')">
        <div class="sb-modal-body">
            <h2>Create ShareBridge Share</h2>

            <label>
                Expiry
                <select data-testid="expiry-select" v-model.number="form.expiryHours">
                    <option v-for="opt in expiryOptions" :key="opt.value" :value="opt.value">
                        {{ opt.label }}
                    </option>
                </select>
            </label>

            <label>
                Password (optional)
                <input data-testid="password-input" v-model="form.password" type="password" />
            </label>

            <label>
                Max Downloads (0 = unlimited)
                <input
                    data-testid="max-downloads-input"
                    v-model.number="form.maxDownloads"
                    type="number"
                    min="0"
                />
            </label>

            <label>
                <input
                    data-testid="relay-only-input"
                    type="checkbox"
                    :checked="form.relayOnly"
                    @change="form.relayOnly = !form.relayOnly"
                />
                Relay mode only
            </label>

            <div v-if="form.relayOnly && !turnAvailable" data-testid="turn-warning" class="sb-warning">
                Warning: Relay mode requires a TURN server. Shares may not connect without one.
            </div>

            <div v-if="error" data-testid="error-msg" class="sb-error">{{ error }}</div>

            <div class="sb-modal-actions">
                <NcButton data-testid="cancel-btn" @click="emit('close')" :disabled="loading">
                    Cancel
                </NcButton>
                <NcButton data-testid="create-btn" @click="submit" :disabled="loading">
                    {{ loading ? 'Creating…' : 'Create Share' }}
                </NcButton>
            </div>
        </div>
    </NcModal>
</template>

<script setup lang="ts">
import { ref, reactive, computed } from 'vue'
import { NcModal, NcButton } from '@nextcloud/vue'
import { useNextcloudOCS } from '../composables/useNextcloudOCS'
import { useAgentClient } from '../composables/useAgentClient'
import type { CreateShareResult } from '../types'

const PRESET_EXPIRY_HOURS = [1, 6, 12, 24, 72, 168, 720]

const props = defineProps<{
    filePath: string           // node.path from @nextcloud/files Node
    turnAvailable?: boolean
    defaultExpiryHours?: number
    defaultMaxDownloads?: number
    defaultRelayOnly?: boolean
}>()
const emit = defineEmits<{
    close:   []
    created: [result: CreateShareResult]
}>()

const { createOCSShare, saveNcShareId } = useNextcloudOCS()
const { createShare }                   = useAgentClient()

const loading = ref(false)
const error   = ref('')
const form    = reactive({
    expiryHours: props.defaultExpiryHours  ?? 24,
    password:    '',
    maxDownloads: props.defaultMaxDownloads ?? 0,
    relayOnly:   props.defaultRelayOnly    ?? false,
})

const expiryOptions = computed(() => {
    const opts = PRESET_EXPIRY_HOURS.map(h => ({ value: h, label: formatExpiry(h) }))
    if (props.defaultExpiryHours && !PRESET_EXPIRY_HOURS.includes(props.defaultExpiryHours)) {
        opts.push({ value: props.defaultExpiryHours, label: `${props.defaultExpiryHours} hours` })
    }
    return opts.sort((a, b) => a.value - b.value)
})

const formatExpiry = (hours: number): string => {
    if (hours < 24) return hours === 1 ? '1 hour' : `${hours} hours`
    const days = hours / 24
    return days === 1 ? '1 day' : `${days} days`
}

const submit = async () => {
    loading.value = true
    error.value   = ''

    try {
        // Step 1: Create Nextcloud public share via OCS (no password needed)
        const { shareUrl, shareId } = await createOCSShare(props.filePath, form.expiryHours)

        // Step 2: Register the share with the agent
        const result = await createShare({
            share_url:     shareUrl,
            password:      form.password || undefined,
            expiry_hours:  form.expiryHours,
            max_downloads: form.maxDownloads,
            relay_only:    form.relayOnly,
        })

        // Step 3: Save the code→ncShareId mapping for future revocation
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