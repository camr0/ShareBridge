<template>
  <div class="modal-overlay" @click.self="emit('close')">
    <div class="modal">
      <h2>Create ShareBridge Share</h2>

      <label>
        Expiry
        <select data-testid="expiry-select" v-model.number="form.expiryHours">
          <option :value="1">1 hour</option>
          <option :value="24">24 hours</option>
          <option :value="168">7 days</option>
          <option :value="720">30 days</option>
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
        <input data-testid="relay-only-input" v-model="form.relayOnly" type="checkbox" />
        Relay mode only
      </label>

      <div
        v-if="form.relayOnly && !turnAvailable"
        data-testid="turn-warning"
        class="warning"
      >
        Warning: Relay mode requires a TURN server. Shares may not connect without one.
      </div>

      <div v-if="error" data-testid="error-msg" class="error">{{ error }}</div>

      <div class="actions">
        <button data-testid="cancel-btn" @click="emit('close')" :disabled="loading">
          Cancel
        </button>
        <button data-testid="create-btn" @click="submit" :disabled="loading">
          {{ loading ? 'Creating...' : 'Create Share' }}
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, computed } from 'vue'
import { useOpenCloudAPI } from '../composables/useOpenCloudAPI'
import { useAgentClient } from '../composables/useAgentClient'
import type { CreateShareResult } from '../types'

const props = defineProps<{
  filePath: string
  turnAvailable?: boolean  // from parent (ShareBridgePanel fetches settings)
}>()
const emit = defineEmits<{
  close: []
  created: [result: CreateShareResult]
}>()

const { createPublicShare } = useOpenCloudAPI()
const { createShare } = useAgentClient()

const loading = ref(false)
const error = ref('')
const form = reactive({
  expiryHours: 24,
  password: '',
  maxDownloads: 0,
  relayOnly: false,
})

const expiryDate = computed(() => {
  const date = new Date()
  date.setHours(date.getHours() + form.expiryHours)
  return date.toISOString().split('T')[0] // YYYY-MM-DD
})

const submit = async () => {
  loading.value = true
  error.value = ''

  let shareUrl: string
  try {
    shareUrl = await createPublicShare(props.filePath, {
      password: form.password || undefined,
      expireDate: expiryDate.value,
    })
  } catch {
    error.value = 'Failed to create OpenCloud share. Please try again.'
    loading.value = false
    return
  }

  try {
    const result = await createShare({
      share_url: shareUrl,
      password: form.password || undefined,
      expiry_hours: form.expiryHours,
      max_downloads: form.maxDownloads,
      relay_only: form.relayOnly,
    })
    emit('created', result)
  } catch {
    error.value = 'Failed to create ShareBridge share. Please try again.'
    loading.value = false
  }
}
</script>