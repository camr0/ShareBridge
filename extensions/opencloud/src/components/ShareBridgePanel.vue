<template>
  <div class="sharebridge-panel">
    <!-- Not configured -->
    <div v-if="!settings.isConfigured" data-testid="configure-prompt" class="configure-prompt">
      <p>Configure ShareBridge to get started.</p>
      <label>
        Agent URL
        <input
          data-testid="agent-url-input"
          v-model="configForm.agentUrl"
          type="url"
          placeholder="http://localhost:7878"
        />
      </label>
      <label>
        API Key
        <input
          data-testid="api-key-input"
          v-model="configForm.apiKey"
          type="password"
          placeholder="sb_agent_..."
        />
      </label>
      <button
        data-testid="save-config-btn"
        @click="saveConfig"
        :disabled="!configForm.agentUrl || !configForm.apiKey"
      >
        Save
      </button>
      <p class="hint">
        Find these values in the ShareBridge agent settings page.
      </p>
    </div>

    <!-- Configured -->
    <template v-else>
      <!-- Error state -->
      <div v-if="error" data-testid="error-msg" class="error">{{ error }}</div>

      <!-- Loading -->
      <div v-else-if="loading" class="loading">Loading shares...</div>

      <!-- Share list -->
      <template v-else>
        <div v-if="shares.length > 0" data-testid="share-list" class="share-list">
          <ShareCard
            v-for="share in shares"
            :key="share.code"
            :share="share"
            @revoke="handleRevoke"
          />
        </div>

        <div v-else data-testid="empty-state" class="empty-state">
          No ShareBridge shares for this file.
        </div>

        <button
          data-testid="create-share-btn"
          @click="showModal = true"
          class="create-btn"
        >
          Create ShareBridge Share
        </button>
      </template>

      <!-- Create modal -->
      <CreateShareModal
        v-if="showModal"
        :resource="resource"
        :turn-available="turnAvailable"
        @close="showModal = false"
        @created="handleCreated"
      />
    </template>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, onMounted, watch } from 'vue'
import { useSettingsStore } from '../stores/settings'
import { useAgentClient } from '../composables/useAgentClient'
import ShareCard from './ShareCard.vue'
import CreateShareModal from './CreateShareModal.vue'
import type { Resource } from '@opencloud-eu/web-client'
import type { Share, CreateShareResult } from '../types'

const props = defineProps<{
  resource: Resource
}>()

const settings = useSettingsStore()
const { listShares, revokeShare, getSettings } = useAgentClient()

const shares = ref<Share[]>([])
const loading = ref(false)
const error = ref('')
const showModal = ref(false)
const turnAvailable = ref(false)

const configForm = reactive({
  agentUrl: '',
  apiKey: '',
})

const saveConfig = async () => {
  if (!configForm.agentUrl || !configForm.apiKey) return
  settings.setAgentUrl(configForm.agentUrl)
  settings.setApiKey(configForm.apiKey)
  // Load shares and fetch settings after saving config
  await loadShares()
  try {
    const agentSettings = await getSettings()
    turnAvailable.value = agentSettings.turn_available
  } catch {
    // leave turnAvailable as false
  }
}

const loadShares = async () => {
  loading.value = true
  error.value = ''
  try {
    console.log('[ShareBridge] loadShares resource:', props.resource)
    shares.value = await listShares(props.resource?.id)
  } catch (err) {
    console.error('[ShareBridge] loadShares error:', err)
    error.value = 'Cannot connect to ShareBridge agent. Check the agent URL and ensure it\'s running.'
  } finally {
    loading.value = false
  }
}

const handleRevoke = async (code: string) => {
  await revokeShare(code)
  await loadShares()
}

const handleCreated = async (_result: CreateShareResult) => {
  showModal.value = false
  await loadShares()
}

// Watch for resource.id becoming available — OpenCloud's sidebar may set the
// resource prop after the component mounts, so onMounted alone is not reliable.
watch(
  () => props.resource?.id,
  (id) => {
    if (id && settings.isConfigured) loadShares()
  },
  { immediate: true }
)

onMounted(async () => {
  if (settings.isConfigured) {
    // Fetch settings for TURN availability (best-effort; non-fatal if it fails)
    try {
      const agentSettings = await getSettings()
      turnAvailable.value = agentSettings.turn_available
    } catch {
      // leave turnAvailable as false — TURN warning will show if relay-only selected
    }
  }
})
</script>

<style scoped>
.sharebridge-panel {
  padding: 12px;
}
.configure-prompt {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.configure-prompt p {
  margin: 0;
  font-size: 0.9em;
}
.configure-prompt label {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 0.9em;
}
.configure-prompt input {
  padding: 6px 8px;
  border-radius: 4px;
  border: 1px solid var(--oc-color-border, #555);
  background: var(--oc-color-background-muted, #2a2a2a);
  color: var(--oc-color-text-default, #fff);
}
.configure-prompt button {
  padding: 6px 16px;
  border-radius: 4px;
  border: 1px solid var(--oc-color-border, #555);
  background: var(--oc-color-swatch-primary-default, #0070f3);
  color: #fff;
  cursor: pointer;
  align-self: flex-start;
}
.configure-prompt button:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.hint {
  margin: 0;
  font-size: 0.8em;
  color: var(--oc-color-text-muted, #aaa);
}
.error {
  padding: 8px 10px;
  background: rgba(220, 50, 50, 0.15);
  border: 1px solid rgba(220, 50, 50, 0.4);
  border-radius: 4px;
  font-size: 0.85em;
  color: #ff6b6b;
}
.loading {
  font-size: 0.9em;
  color: var(--oc-color-text-muted, #aaa);
}
.empty-state {
  font-size: 0.9em;
  color: var(--oc-color-text-muted, #aaa);
  margin-bottom: 12px;
}
.share-list {
  margin-bottom: 12px;
}
.create-btn {
  width: 100%;
  padding: 8px;
  border-radius: 4px;
  border: 1px solid var(--oc-color-border, #555);
  background: var(--oc-color-swatch-primary-default, #0070f3);
  color: #fff;
  cursor: pointer;
  font-size: 0.9em;
}
.create-btn:hover {
  opacity: 0.9;
}
</style>