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

const formattedExpiry = computed(() => {
  const expires = new Date(props.share.expires_at)
  const now = new Date()
  if (expires <= now) return 'Expired'
  const hours = Math.floor((expires.getTime() - now.getTime()) / 3600000)
  if (hours >= 24) {
    const days = Math.floor(hours / 24)
    return days === 1 ? '1 day' : `${days} days`
  }
  if (hours >= 1) return hours === 1 ? '1 hour' : `${hours} hours`
  const minutes = Math.floor((expires.getTime() - now.getTime()) / 60000)
  return minutes <= 1 ? '<1 min' : `${minutes} min`
})

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

<style scoped>
.share-card {
  border: 1px solid var(--oc-color-border, #444);
  border-radius: 6px;
  padding: 10px 12px;
  margin-bottom: 8px;
}
.share-info {
  display: flex;
  flex-direction: column;
  gap: 2px;
  margin-bottom: 8px;
  font-size: 0.85em;
}
.share-code {
  font-family: monospace;
  font-weight: bold;
}
.share-downloads, .share-expiry {
  color: var(--oc-color-text-muted, #aaa);
}
.share-actions {
  display: flex;
  gap: 8px;
}
.share-actions button {
  padding: 4px 10px;
  border-radius: 4px;
  border: 1px solid var(--oc-color-border, #555);
  background: var(--oc-color-background-muted, #333);
  color: var(--oc-color-text-default, #fff);
  cursor: pointer;
  font-size: 0.85em;
}
.share-actions button:hover {
  background: var(--oc-color-background-hover, #444);
}
</style>