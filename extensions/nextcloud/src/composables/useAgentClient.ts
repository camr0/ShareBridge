import type { CreateShareParams, CreateShareResult, Share, AgentSettings } from '../types'
import { useSettingsStore } from '../stores/settings'

export const useAgentClient = () => {
	const settings = useSettingsStore()

	const getHeaders = (): Record<string, string> => ({
		'Content-Type': 'application/json',
		'X-API-Key': settings.apiKey,
	})

	const listShares = async (fileId?: string): Promise<Share[]> => {
		const url = new URL(`${settings.agentUrl}/api/v1/shares`)
		if (fileId) url.searchParams.set('file_id', fileId)
		const response = await fetch(url.toString(), { headers: getHeaders() })
		return response.json()
	}

	const createShare = async (params: CreateShareParams): Promise<CreateShareResult> => {
		const response = await fetch(`${settings.agentUrl}/api/v1/shares`, {
			method: 'POST',
			headers: getHeaders(),
			body: JSON.stringify(params),
		})
		return response.json()
	}

	const revokeShare = async (code: string): Promise<void> => {
		await fetch(`${settings.agentUrl}/api/v1/shares/${code}`, {
			method: 'DELETE',
			headers: getHeaders(),
		})
	}

	const getSettings = async (): Promise<AgentSettings> => {
		const response = await fetch(`${settings.agentUrl}/api/v1/settings`, {
			headers: getHeaders(),
		})
		return response.json()
	}

	return { listShares, createShare, revokeShare, getSettings }
}