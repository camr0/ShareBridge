<template>
  <div class="share-card">
    <div class="share-info">
      <span class="share-code">{{ share.code }}</span>
      <span class="share-downloads">
        {{ share.downloads }} / {{ share.max_downloads === 0 ? '∞' : share.max_downloads }} downloads
      </span>
      <span class="share-expiry">Expires: {{ formattedExpiry }}</span>
    </div>
    <div class="share-actions">
      <button data-testid="copy-btn" @click="copyLink">
        {{ copied ? 'Copied!' : 'Copy Link' }}
      </button>
      <button data-testid="revoke-btn" @click="emit('revoke', share.code)">
        Revoke
      </button>
    </div>
    <div v-if="copied" data-testid="copied-feedback" class="copied-feedback">
      Copied!
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onUnmounted } from 'vue'
import type { Share } from '../types'

const props = defineProps<{ share: Share }>()
const emit = defineEmits<{ revoke: [code: string] }>()

const copied = ref(false)
let copyTimer: ReturnType<typeof setTimeout> | null = null

const formattedExpiry = computed(() =>
  new Date(props.share.expires_at).toLocaleDateString()
)

const copyLink = async () => {
  await navigator.clipboard.writeText(props.share.public_url)
  copied.value = true
  if (copyTimer) clearTimeout(copyTimer)
  copyTimer = setTimeout(() => { copied.value = false }, 2000)
}

onUnmounted(() => {
  if (copyTimer) clearTimeout(copyTimer)
})
</script>