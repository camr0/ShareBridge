import { describe, it, expect, vi, beforeEach } from 'vitest'
import { setActivePinia, createPinia } from 'pinia'
import axios from '@nextcloud/axios'
import { useSettingsStore } from './settings'

describe('useSettingsStore', () => {
    beforeEach(() => {
        setActivePinia(createPinia())
        vi.mocked(axios.get).mockReset()
        vi.mocked(axios.put).mockReset()
    })

    it('starts in loading=false, loaded=false, empty strings', () => {
        const store = useSettingsStore()
        expect(store.agentUrl).toBe('')
        expect(store.apiKey).toBe('')
        expect(store.loading).toBe(false)
        expect(store.loaded).toBe(false)
    })

    it('isConfigured is false when not yet loaded', () => {
        const store = useSettingsStore()
        expect(store.isConfigured).toBe(false)
    })

    it('fetchSettings: sets loading=true during fetch, then false', async () => {
        let resolveGet!: (v: unknown) => void
        vi.mocked(axios.get).mockReturnValue(new Promise(r => { resolveGet = r }) as never)

        const store = useSettingsStore()
        const fetchPromise = store.fetchSettings()

        expect(store.loading).toBe(true)

        resolveGet({ data: { agent_url: 'http://localhost:7878', api_key: 'sb_key' } })
        await fetchPromise

        expect(store.loading).toBe(false)
    })

    it('fetchSettings: populates agentUrl and apiKey from response', async () => {
        vi.mocked(axios.get).mockResolvedValue({
            data: { agent_url: 'http://localhost:7878', api_key: 'sb_agent_abc' },
        } as never)

        const store = useSettingsStore()
        await store.fetchSettings()

        expect(store.agentUrl).toBe('http://localhost:7878')
        expect(store.apiKey).toBe('sb_agent_abc')
        expect(store.loaded).toBe(true)
    })

    it('fetchSettings: isConfigured is true after fetching non-empty values', async () => {
        vi.mocked(axios.get).mockResolvedValue({
            data: { agent_url: 'http://localhost:7878', api_key: 'sb_agent_abc' },
        } as never)

        const store = useSettingsStore()
        await store.fetchSettings()

        expect(store.isConfigured).toBe(true)
    })

    it('fetchSettings: isConfigured stays false when values are empty', async () => {
        vi.mocked(axios.get).mockResolvedValue({
            data: { agent_url: '', api_key: '' },
        } as never)

        const store = useSettingsStore()
        await store.fetchSettings()

        expect(store.isConfigured).toBe(false)
    })

    it('fetchSettings: on error, stays unconfigured, loading=false, loaded=true', async () => {
        vi.mocked(axios.get).mockRejectedValue(new Error('Network error'))

        const store = useSettingsStore()
        await store.fetchSettings()

        expect(store.agentUrl).toBe('')
        expect(store.apiKey).toBe('')
        expect(store.loading).toBe(false)
        expect(store.loaded).toBe(true)
    })

    it('fetchSettings: does not re-fetch if already loaded', async () => {
        vi.mocked(axios.get).mockResolvedValue({
            data: { agent_url: 'http://localhost:7878', api_key: 'sb_key' },
        } as never)

        const store = useSettingsStore()
        await store.fetchSettings()
        await store.fetchSettings() // second call

        expect(vi.mocked(axios.get)).toHaveBeenCalledTimes(1)
    })

    it('saveSettings: PUTs current agentUrl and apiKey to the endpoint', async () => {
        vi.mocked(axios.put).mockResolvedValue({ data: { status: 'ok' } } as never)

        const store = useSettingsStore()
        store.$patch({ agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })
        await store.saveSettings()

        expect(vi.mocked(axios.put)).toHaveBeenCalledWith(
            '/apps/sharebridge/api/settings',
            { agent_url: 'http://localhost:7878', api_key: 'sb_key' }
        )
    })
})