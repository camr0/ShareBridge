<template>
    <div class="sb-share-card">
        <div class="sb-share-info">
            <span class="sb-share-code" data-testid="share-code">{{ share.code }}</span>
            <span class="sb-share-downloads">
                {{ share.downloads }} / {{ share.max_downloads === 0 ? '∞' : share.max_downloads }} downloads
            </span>
            <span class="sb-share-expiry">Expires: {{ formattedExpiry }}</span>
        </div>
        <div class="sb-share-actions">
            <NcButton data-testid="copy-btn" @click="copyLink">
                {{ copied ? 'Copied!' : 'Copy Link' }}
            </NcButton>
            <NcButton data-testid="revoke-btn" @click="emit('revoke', share.code)">
                Revoke
            </NcButton>
        </div>
    </div>
</template>

<script setup lang="ts">
import { ref, computed, onUnmounted } from 'vue'
import { NcButton } from '@nextcloud/vue'
import type { Share } from '../types'

const props = defineProps<{ share: Share }>()
const emit  = defineEmits<{ revoke: [code: string] }>()

const copied = ref(false)
let copyTimer: ReturnType<typeof setTimeout> | null = null

const formattedExpiry = computed(() => {
    const expires = new Date(props.share.expires_at)
    const now     = new Date()
    if (expires <= now) return 'Expired'
    const hours = Math.floor((expires.getTime() - now.getTime()) / 3_600_000)
    if (hours >= 24) {
        const days = Math.floor(hours / 24)
        return days === 1 ? '1 day' : `${days} days`
    }
    if (hours >= 1) return hours === 1 ? '1 hour' : `${hours} hours`
    const minutes = Math.floor((expires.getTime() - now.getTime()) / 60_000)
    return minutes <= 1 ? '<1 min' : `${minutes} min`
})

const copyLink = async () => {
    await navigator.clipboard.writeText(props.share.public_url)
    copied.value = true
    if (copyTimer) clearTimeout(copyTimer)
    copyTimer = setTimeout(() => { copied.value = false }, 2000)
}

onUnmounted(() => { if (copyTimer) clearTimeout(copyTimer) })
</script>