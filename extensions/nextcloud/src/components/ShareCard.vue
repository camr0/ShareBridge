<template>
	<div class="sb-share-card">
		<div class="sb-share-header">
			<span class="sb-share-code" data-testid="share-code">{{ share.code }}</span>
			<span
				class="sb-connection-badge"
				:class="share.relay_only ? 'sb-connection-badge--relay' : 'sb-connection-badge--direct'"
				data-testid="connection-badge"
			>
				{{ share.relay_only ? 'Relay' : 'Direct' }}
			</span>
		</div>
		<div class="sb-share-meta" data-testid="share-meta">
			<span>{{ share.downloads }} / {{ share.max_downloads === 0 ? '∞' : share.max_downloads }} downloads</span>
			<span>·</span>
			<span>{{ formattedExpiry }}</span>
		</div>
		<div class="sb-share-actions">
			<NcButton data-testid="copy-btn" @click="copyLink">
				{{ copied ? 'Copied!' : 'Copy Link' }}
			</NcButton>
			<NcButton data-testid="revoke-btn" type="error" @click="emit('revoke', share.code)">
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
const emit = defineEmits<{ revoke: [code: string] }>()

const copied = ref(false)
let copyTimer: ReturnType<typeof setTimeout> | null = null

const formattedExpiry = computed(() => {
	const expires = new Date(props.share.expires_at)
	const now = new Date()
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

<style scoped>
.sb-share-card {
	display: flex;
	flex-direction: column;
	gap: 8px;
	padding: 12px;
	border-radius: var(--border-radius-large, 8px);
	border: 1px solid var(--color-border);
	background: var(--color-background-hover);
}

.sb-share-header {
	display: flex;
	align-items: center;
	gap: 8px;
}

.sb-share-code {
	font-weight: 600;
	font-size: 14px;
	font-family: var(--font-face-monospace, monospace);
	flex: 1;
	min-width: 0;
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
}

.sb-connection-badge {
	display: inline-flex;
	align-items: center;
	padding: 2px 8px;
	border-radius: 100px;
	font-size: 11px;
	font-weight: 600;
	flex-shrink: 0;
}

.sb-connection-badge--direct {
	background: color-mix(in srgb, var(--color-success) 15%, transparent);
	color: var(--color-success);
	border: 1px solid color-mix(in srgb, var(--color-success) 40%, transparent);
}

.sb-connection-badge--relay {
	background: color-mix(in srgb, var(--color-warning) 15%, transparent);
	color: var(--color-warning-text, var(--color-warning));
	border: 1px solid color-mix(in srgb, var(--color-warning) 40%, transparent);
}

.sb-share-meta {
	display: flex;
	gap: 6px;
	font-size: 12px;
	color: var(--color-text-maxcontrast);
}

.sb-share-actions {
	display: flex;
	gap: 4px;
}
</style>
