<template>
  <Teleport to="body">
  <div class="sb-modal-overlay" @click.self="emit('close')">
      <div class="sb-modal">
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

        <fieldset class="sb-connection-mode">
          <legend>Connection Mode</legend>

          <label data-testid="relay-option" class="sb-mode-option">
            <input
              data-testid="mode-relay"
              type="radio"
              :checked="form.relayOnly"
              @change="form.relayOnly = true"
            />
            <span>
              <strong>Relay (recommended)</strong>
              <span>End-to-end encrypted, hides your IP, and provides consistent performance</span>
            </span>
          </label>

          <label data-testid="direct-option" class="sb-mode-option">
            <input
              data-testid="mode-direct"
              type="radio"
              :checked="!form.relayOnly"
              @change="form.relayOnly = false"
            />
            <span>
              <strong>Direct</strong>
              <span>Peer-to-peer, quota-free</span>
            </span>
          </label>
        </fieldset>

        <div v-if="!form.relayOnly" data-testid="direct-advisory" class="sb-warning">
          Direct transfers expose your IP address and may be slower due to browser protocol limitations. Use Relay for more consistent performance.
        </div>

        <div v-if="error" data-testid="error-msg" class="sb-error">{{ error }}</div>

        <div class="sb-actions">
          <button data-testid="cancel-btn" @click="emit('close')" :disabled="loading">
            Cancel
          </button>
          <button data-testid="create-btn" @click="submit" :disabled="loading">
            {{ loading ? 'Creating...' : 'Create Share' }}
          </button>
        </div>
      </div>
    </div>
  </Teleport>
</template>

<script setup lang="ts">
import { ref, reactive, computed } from 'vue'
import { useClientService } from '@opencloud-eu/web-pkg'
import { useAgentClient } from '../composables/useAgentClient'
import type { Resource } from '@opencloud-eu/web-client'
import type { CreateShareResult } from '../types'

const PRESET_EXPIRY_OPTIONS = [1, 6, 12, 24, 72, 168, 720] // hours: 1h, 6h, 12h, 24h, 3d, 7d, 30d (matches agent UI)

const props = withDefaults(defineProps<{
  resource: Resource
  turnAvailable?: boolean
  defaultExpiryHours?: number
  defaultMaxDownloads?: number
  defaultRelayOnly?: boolean
}>(), {
  defaultRelayOnly: undefined,
})
const emit = defineEmits<{
  close: []
  created: [result: CreateShareResult]
}>()

const clientService = useClientService()
const { createShare } = useAgentClient()

const loading = ref(false)
const error = ref('')
const form = reactive({
  expiryHours: props.defaultExpiryHours ?? 24,
  password: '',
  maxDownloads: props.defaultMaxDownloads ?? 0,
  relayOnly: props.defaultRelayOnly ?? true,
})

// Include agent default in expiry options if it's not a preset
const expiryOptions = computed(() => {
  const options = PRESET_EXPIRY_OPTIONS.map(h => ({ value: h, label: formatExpiryLabel(h) }))
  if (props.defaultExpiryHours && !PRESET_EXPIRY_OPTIONS.includes(props.defaultExpiryHours)) {
    options.push({ value: props.defaultExpiryHours, label: `${props.defaultExpiryHours} hours` })
  }
  return options.sort((a, b) => a.value - b.value)
})

const formatExpiryLabel = (hours: number): string => {
  if (hours === 1) return '1 hour'
  if (hours === 6) return '6 hours'
  if (hours === 12) return '12 hours'
  if (hours === 24) return '24 hours'
  if (hours === 72) return '3 days'
  if (hours === 168) return '7 days'
  if (hours === 720) return '30 days'
  return `${hours} hours`
}

const expiryDate = computed(() => {
  const date = new Date()
  date.setHours(date.getHours() + form.expiryHours)
  return date.toISOString() // full ISO 8601 e.g. "2026-04-12T10:30:00.000Z"
})

const submit = async () => {
  loading.value = true
  error.value = ''

  const driveId = props.resource.storageId!
  const itemId = props.resource.id
  const tempPassword = 'ShareBridge123$'

  let shareUrl: string
  let permId: string
  try {
    // Step 1: create the link with a temporary password (server enforces password on creation)
    const linkShare = await clientService.graphAuthenticated.permissions.createLink(
      driveId,
      itemId,
      {
        type: 'view',
        password: tempPassword,
        expirationDateTime: expiryDate.value,
      }
    )
    shareUrl = linkShare.webUrl
    permId = linkShare.id
  } catch (err: unknown) {
    const axiosErr = err as { response?: { data?: unknown; status?: number } }
    const msg = (axiosErr?.response?.data as { error?: { message?: string } })?.error?.message
    error.value = msg ? `OpenCloud: ${msg}` : 'Failed to create OpenCloud share.'
    loading.value = false
    return
  }

  try {
    // Step 2: remove the temporary password so the link is public
    await clientService.graphAuthenticated.permissions.setPermissionPassword(
      driveId,
      itemId,
      permId,
      { password: '' }
    )
  } catch {
    // Non-fatal: link still works but has a temp password. Continue.
  }

  try {
    const result = await createShare({
      share_url: shareUrl,
      password: form.password || undefined,
      expiry_hours: form.expiryHours,
      max_downloads: form.maxDownloads,
      relay_only: form.relayOnly,
})
    loading.value = false
    emit('created', result)
  } catch {
    error.value = 'Failed to create ShareBridge share. Please try again.'
    loading.value = false
  }
}

</script>

<style scoped>
.sb-modal-overlay {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.6);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 9999;
}
.sb-modal {
  background: var(--oc-color-background-default, #1e1e1e);
  border: 1px solid var(--oc-color-border, #444);
  border-radius: 8px;
  padding: 24px;
  width: 340px;
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.sb-modal h2 {
  margin: 0 0 4px;
  font-size: 1.1em;
}
.sb-modal label {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 0.9em;
}
.sb-modal input, .sb-modal select {
  padding: 6px 8px;
  border-radius: 4px;
  border: 1px solid var(--oc-color-border, #555);
  background: var(--oc-color-background-muted, #2a2a2a);
  color: var(--oc-color-text-default, #fff);
}
.sb-connection-mode {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin: 0;
  padding: 0;
  border: 0;
}
.sb-connection-mode legend {
  margin-bottom: 4px;
  font-size: 0.9em;
}
.sb-modal .sb-mode-option {
  flex-direction: row;
  align-items: flex-start;
  gap: 8px;
  cursor: pointer;
}
.sb-mode-option input {
  margin: 3px 0 0;
}
.sb-mode-option span {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.sb-warning {
  background: rgba(255, 180, 0, 0.15);
  border: 1px solid rgba(255, 180, 0, 0.4);
  border-radius: 4px;
  padding: 8px 10px;
  font-size: 0.85em;
  color: #ffb400;
}
.sb-error {
  background: rgba(220, 50, 50, 0.15);
  border: 1px solid rgba(220, 50, 50, 0.4);
  border-radius: 4px;
  padding: 8px 10px;
  font-size: 0.85em;
  color: #ff6b6b;
}
.sb-actions {
  display: flex;
  gap: 8px;
  justify-content: flex-end;
}
.sb-actions button {
  padding: 6px 16px;
  border-radius: 4px;
  border: 1px solid var(--oc-color-border, #555);
  background: var(--oc-color-background-muted, #333);
  color: var(--oc-color-text-default, #fff);
  cursor: pointer;
}
.sb-actions button:last-child {
  background: var(--oc-color-swatch-primary-default, #0070f3);
  border-color: var(--oc-color-swatch-primary-default, #0070f3);
}
.sb-actions button:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
</style>
