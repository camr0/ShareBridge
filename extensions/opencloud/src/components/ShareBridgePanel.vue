<template>
  <div class="sharebridge-panel">
    <!-- Not configured -->
    <div v-if="!settings.isConfigured" data-testid="configure-prompt" class="configure-prompt">
      <p>Configure ShareBridge to get started.</p>
      <p>
        Add your Agent URL and API Key in the OpenCloud extension settings.<br />
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
import { ref, onMounted } from 'vue'
import { useSettingsStore } from '../stores/settings'
import { useAgentClient } from '../composables/useAgentClient'
import ShareCard from './ShareCard.vue'
import CreateShareModal from './CreateShareModal.vue'
import type { Share, CreateShareResult } from '../types'

// The resource prop comes from OpenCloud's sidebar context.
// Field name 'resource' should be verified against your @opencloud-eu/web-pkg version.
// It may be passed differently depending on how the SidebarPanelExtension mounts the component.
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