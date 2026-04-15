import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { setActivePinia, createPinia } from 'pinia'
import { useAgentClient } from './useAgentClient'
import { useSettingsStore } from '../stores/settings'

describe('useAgentClient', () => {
	let mockFetch: ReturnType<typeof vi.fn>

	beforeEach(() => {
		setActivePinia(createPinia())
		mockFetch = vi.fn()
		vi.stubGlobal('fetch', mockFetch)
	})

	afterEach(() => {
		vi.unstubAllGlobals()
	})

	const setupStore = (url = 'http://localhost:7878', key = 'sb_agent_testkey') => {
		const store = useSettingsStore()
		store.$patch({ agentUrl: url, apiKey: key })
		return store
	}

	it('listShares fetches /api/v1/shares with X-API-Key header', async () => {
		setupStore()
		mockFetch.mockResolvedValue({ json: () => Promise.resolve([]) })

		const { listShares } = useAgentClient()
		await listShares()

		expect(mockFetch).toHaveBeenCalledWith(
			'http://localhost:7878/api/v1/shares',
			expect.objectContaining({
				headers: expect.objectContaining({ 'X-API-Key': 'sb_agent_testkey' }),
			})
		)
	})

	it('listShares appends file_id query param when provided', async () => {
		setupStore()
		mockFetch.mockResolvedValue({ json: () => Promise.resolve([]) })

		const { listShares } = useAgentClient()
		await listShares('12345')

		expect(mockFetch).toHaveBeenCalledWith(
			'http://localhost:7878/api/v1/shares?file_id=12345',
			expect.anything()
		)
	})

	it('createShare POSTs JSON to /api/v1/shares', async () => {
		setupStore()
		const mockResult = { code: 'abc123', public_url: 'https://share.example.com/s/abc123', expires_at: '2026-04-15T00:00:00Z' }
		mockFetch.mockResolvedValue({ json: () => Promise.resolve(mockResult) })

		const { createShare } = useAgentClient()
		const result = await createShare({
			share_url: 'https://nextcloud.example.com/s/XYZ789',
			expiry_hours: 24,
			max_downloads: 0,
			relay_only: false,
		})

		expect(mockFetch).toHaveBeenCalledWith(
			'http://localhost:7878/api/v1/shares',
			expect.objectContaining({ method: 'POST' })
		)
		expect(result).toEqual(mockResult)
	})

	it('revokeShare sends DELETE to /api/v1/shares/{code}', async () => {
		setupStore()
		mockFetch.mockResolvedValue({})

		const { revokeShare } = useAgentClient()
		await revokeShare('abc123')

		expect(mockFetch).toHaveBeenCalledWith(
			'http://localhost:7878/api/v1/shares/abc123',
			expect.objectContaining({ method: 'DELETE' })
		)
	})

	it('getSettings fetches /api/v1/settings with X-API-Key', async () => {
		setupStore()
		const mockSettings = { default_expiry_hours: 24, default_max_downloads: 0, default_relay_only: false, turn_available: true }
		mockFetch.mockResolvedValue({ json: () => Promise.resolve(mockSettings) })

		const { getSettings } = useAgentClient()
		const result = await getSettings()

		expect(mockFetch).toHaveBeenCalledWith(
			'http://localhost:7878/api/v1/settings',
			expect.objectContaining({ headers: expect.objectContaining({ 'X-API-Key': 'sb_agent_testkey' }) })
		)
		expect(result).toEqual(mockSettings)
	})
})