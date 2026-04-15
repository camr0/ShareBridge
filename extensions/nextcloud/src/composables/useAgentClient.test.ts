import { describe, it, expect, vi, beforeEach } from 'vitest'
import axios from '@nextcloud/axios'
import { useAgentClient } from './useAgentClient'

vi.mock('@nextcloud/axios', () => ({
    default: {
        get: vi.fn(),
        post: vi.fn(),
        delete: vi.fn(),
    },
}))

vi.mock('@nextcloud/router', () => ({
    generateUrl: (path: string) => `https://nc.example.com${path}`,
}))

describe('useAgentClient', () => {
    beforeEach(() => {
        vi.clearAllMocks()
    })

    it('listShares fetches the Nextcloud proxy shares endpoint', async () => {
        vi.mocked(axios.get).mockResolvedValue({ data: [] } as never)

        const { listShares } = useAgentClient()
        await listShares()

        expect(axios.get).toHaveBeenCalledWith(
            'https://nc.example.com/apps/sharebridge/api/agent/shares',
            { params: {} }
        )
    })

    it('listShares appends file_id query param when provided', async () => {
        vi.mocked(axios.get).mockResolvedValue({ data: [] } as never)

        const { listShares } = useAgentClient()
        await listShares('12345')

        expect(axios.get).toHaveBeenCalledWith(
            'https://nc.example.com/apps/sharebridge/api/agent/shares',
            { params: { file_id: '12345' } }
        )
    })

    it('createShare POSTs JSON to the Nextcloud proxy shares endpoint', async () => {
        const mockResult = { code: 'abc123', public_url: 'https://share.example.com/s/abc123', expires_at: '2026-04-15T00:00:00Z' }
        vi.mocked(axios.post).mockResolvedValue({ data: mockResult } as never)

        const { createShare } = useAgentClient()
        const result = await createShare({
            share_url: 'https://nextcloud.example.com/s/XYZ789',
            expiry_hours: 24,
            max_downloads: 0,
            relay_only: false,
        })

        expect(axios.post).toHaveBeenCalledWith(
            'https://nc.example.com/apps/sharebridge/api/agent/shares',
            expect.objectContaining({
                share_url: 'https://nextcloud.example.com/s/XYZ789',
                expiry_hours: 24,
                max_downloads: 0,
                relay_only: false,
            })
        )
        expect(result).toEqual(mockResult)
    })

    it('createShare sends share_type: nextcloud', async () => {
        const mockResult = { code: 'abc123', public_url: 'https://share.example.com/s/abc123', expires_at: '2026-04-15T00:00:00Z' }
        vi.mocked(axios.post).mockResolvedValue({ data: mockResult } as never)

        const { createShare } = useAgentClient()
        await createShare({
            share_url: 'https://nextcloud.example.com/s/XYZ789',
            expiry_hours: 24,
            max_downloads: 0,
            relay_only: false,
        })

        expect(axios.post).toHaveBeenCalledWith(
            expect.any(String),
            expect.objectContaining({
                share_type: 'nextcloud',
            }),
        )
    })

    it('revokeShare sends DELETE to the Nextcloud proxy shares endpoint', async () => {
        vi.mocked(axios.delete).mockResolvedValue({} as never)

        const { revokeShare } = useAgentClient()
        await revokeShare('abc123')

        expect(axios.delete).toHaveBeenCalledWith(
            'https://nc.example.com/apps/sharebridge/api/agent/shares/abc123'
        )
    })

    it('getSettings fetches the Nextcloud proxy settings endpoint', async () => {
        const mockSettings = { default_expiry_hours: 24, default_max_downloads: 0, default_relay_only: false, turn_available: true }
        vi.mocked(axios.get).mockResolvedValue({ data: mockSettings } as never)

        const { getSettings } = useAgentClient()
        const result = await getSettings()

        expect(axios.get).toHaveBeenCalledWith(
            'https://nc.example.com/apps/sharebridge/api/agent/settings'
        )
        expect(result).toEqual(mockSettings)
    })
})
