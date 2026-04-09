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
        :file-path="resource.path"
        :turn-available="turnAvailable"
        @close="showModal = false"
        @created="handleCreated"
      />
    </template>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, onMounted } from 'vue'
import { useSettingsStore } from '../stores/settings'
import { useAgentClient } from '../composables/useAgentClient'
import ShareCard from './ShareCard.vue'
import CreateShareModal from './CreateShareModal.vue'
import type { Share, CreateShareResult } from '../types'

const props = defineProps<{
  resource: {
    id: string    // OpenCloud fileId (oc:fileid)
    path: string  // File path, used for OCS share creation
    name: string
  }
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
    shares.value = await listShares(props.resource.id)
  } catch {
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

onMounted(async () => {
  if (settings.isConfigured) {
    loadShares()
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