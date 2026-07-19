import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { setActivePinia, createPinia } from 'pinia'
import { useSettingsStore } from '../stores/settings'
import ShareBridgeTab from './ShareBridgeTab.vue'
import type { Share } from '../types'

// Create mutable mock functions that can be controlled in tests
const mockListShares     = vi.fn().mockResolvedValue([])
const mockRevokeShare    = vi.fn().mockResolvedValue(undefined)
const mockGetSettings    = vi.fn().mockResolvedValue({ default_expiry_hours: 24, default_max_downloads: 0, default_relay_only: false, turn_available: false })
const mockGetNcShareId   = vi.fn().mockResolvedValue('42')
const mockDeleteOCSShare = vi.fn().mockResolvedValue(undefined)
const mockDeleteNcShareId = vi.fn().mockResolvedValue(undefined)

// Mock composables so tests don't hit real network
vi.mock('../composables/useAgentClient', () => ({
    useAgentClient: () => ({
        listShares:  mockListShares,
        revokeShare: mockRevokeShare,
        getSettings: mockGetSettings,
    }),
}))
vi.mock('../composables/useNextcloudOCS', () => ({
    useNextcloudOCS: () => ({
        getNcShareId:    mockGetNcShareId,
        deleteOCSShare:  mockDeleteOCSShare,
        deleteNcShareId: mockDeleteNcShareId,
    }),
}))

// id is always a string — String(fileid) for v3 path, INode.id for v4 path
const makeNode = (fileid = 12345, path = '/Documents/report.pdf') => ({ id: String(fileid), path })

const makeShare = (code = 'ABC123'): Share => ({
    code,
    public_url:    `https://share.example.com/s/${code}`,
    share_url:     'https://nc.example.com/s/XYZ',
    file_id:       '12345',
    downloads:     0,
    max_downloads: 0,
    relay_only:    false,
    expires_at:    new Date(Date.now() + 86_400_000).toISOString(),
    created_at:    new Date().toISOString(),
})

describe('ShareBridgeTab', () => {
    beforeEach(() => {
        setActivePinia(createPinia())
        // Reset mock call counts
        mockListShares.mockClear()
        mockRevokeShare.mockClear()
        mockGetSettings.mockClear()
        mockGetNcShareId.mockClear()
        mockDeleteOCSShare.mockClear()
        mockDeleteNcShareId.mockClear()
        // Reset to default implementations
        mockListShares.mockResolvedValue([])
        mockGetSettings.mockResolvedValue({ default_expiry_hours: 24, default_max_downloads: 0, default_relay_only: false, turn_available: false })
        mockGetNcShareId.mockResolvedValue('42')
    })

    it('shows "Configure in Personal Settings" prompt when not configured', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: '', apiKey: '' })

        const wrapper = mount(ShareBridgeTab, { props: { node: makeNode() } })
        await flushPromises()

        expect(wrapper.text()).toContain('Personal Settings')
        expect(wrapper.find('[data-testid="configure-prompt"]').exists()).toBe(true)
    })

    it('shows loading state while settings are being fetched', () => {
        useSettingsStore().$patch({ loaded: false, loading: true })

        const wrapper = mount(ShareBridgeTab, { props: { node: makeNode() } })
        expect(wrapper.find('.nc-loading-icon').exists()).toBe(true)
    })

    it('calls listShares with node fileid when configured', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })

        mount(ShareBridgeTab, { props: { node: makeNode(99999) } })
        await flushPromises()

        expect(mockListShares).toHaveBeenCalledWith('99999') // String(99999)
    })

    it('shows empty state when no shares exist for this file', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })

        const wrapper = mount(ShareBridgeTab, { props: { node: makeNode() } })
        await flushPromises()

        expect(wrapper.find('[data-testid="empty-state"]').exists()).toBe(true)
    })

    it('renders a ShareCard for each share', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })
        mockListShares.mockResolvedValue([makeShare('ABC'), makeShare('XYZ')])

        const wrapper = mount(ShareBridgeTab, { props: { node: makeNode() } })
        await flushPromises()

        expect(wrapper.findAll('[data-testid="share-card"]').length).toBe(2)
    })

    it('shows Create button when configured', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })

        const wrapper = mount(ShareBridgeTab, { props: { node: makeNode() } })
        await flushPromises()

        expect(wrapper.find('[data-testid="create-share-btn"]').exists()).toBe(true)
    })

    it('passes no relay default when agent settings fail to load', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })
        mockGetSettings.mockRejectedValueOnce(new Error('Network error'))

        const wrapper = mount(ShareBridgeTab, { props: { node: makeNode() } })
        await flushPromises()
        await wrapper.find('[data-testid="create-share-btn"]').trigger('click')

        const modal = wrapper.findComponent({ name: 'CreateShareModal' })
        expect(modal.props('defaultRelayOnly')).toBeUndefined()
        expect(modal.find<HTMLInputElement>('[data-testid="mode-relay"]').element.checked).toBe(true)
    })

    it('waits for agent settings before opening with the saved Direct preference', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })
        let resolveSettings: (settings: {
            default_expiry_hours: number
            default_max_downloads: number
            default_relay_only: boolean
            turn_available: boolean
        }) => void
        mockGetSettings.mockImplementationOnce(() => new Promise(resolve => { resolveSettings = resolve }))

        const wrapper = mount(ShareBridgeTab, { props: { node: makeNode() } })
        await flushPromises()

        const createButton = wrapper.find('[data-testid="create-share-btn"]')
        await createButton.trigger('click')
        expect(wrapper.findComponent({ name: 'CreateShareModal' }).exists()).toBe(false)

        resolveSettings!({
            default_expiry_hours: 24,
            default_max_downloads: 0,
            default_relay_only: false,
            turn_available: false,
        })
        await flushPromises()
        await createButton.trigger('click')

        const modal = wrapper.findComponent({ name: 'CreateShareModal' })
        expect(modal.exists()).toBe(true)
        expect(modal.find<HTMLInputElement>('[data-testid="mode-direct"]').element.checked).toBe(true)
    })

    it('preserves an explicitly false relay default from agent settings', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })

        const wrapper = mount(ShareBridgeTab, { props: { node: makeNode() } })
        await flushPromises()
        await wrapper.find('[data-testid="create-share-btn"]').trigger('click')

        expect(wrapper.findComponent({ name: 'CreateShareModal' }).props('defaultRelayOnly')).toBe(false)
    })

    it('shows connection error when listShares throws', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })
        mockListShares.mockRejectedValueOnce(new Error('Network error'))

        const wrapper = mount(ShareBridgeTab, { props: { node: makeNode() } })
        await flushPromises()

        expect(wrapper.find('[data-testid="error-msg"]').exists()).toBe(true)
        expect(wrapper.find('[data-testid="error-msg"]').text()).toContain('Cannot connect')
    })

    it('shows error when revoke fails', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })
        mockListShares.mockResolvedValue([makeShare('ABC123')])
        mockDeleteOCSShare.mockRejectedValueOnce(new Error('OCS error'))

        const wrapper = mount(ShareBridgeTab, { props: { node: makeNode() } })
        await flushPromises()

        wrapper.findComponent({ name: 'ShareCard' }).vm.$emit('revoke', 'ABC123')
        await flushPromises()

        expect(wrapper.find('[data-testid="error-msg"]').exists()).toBe(true)
        expect(wrapper.find('[data-testid="error-msg"]').text()).toContain('Failed to fully revoke')
    })

    it('on revoke: calls getNcShareId, revokeShare, deleteOCSShare, then reloads shares', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })
        mockListShares.mockResolvedValue([makeShare('ABC123')])

        const wrapper = mount(ShareBridgeTab, { props: { node: makeNode() } })
        await flushPromises()

        // Trigger revoke from the first ShareCard
        wrapper.findComponent({ name: 'ShareCard' }).vm.$emit('revoke', 'ABC123')
        await flushPromises()

        expect(mockGetNcShareId).toHaveBeenCalledWith('ABC123')
        expect(mockRevokeShare).toHaveBeenCalledWith('ABC123')
        expect(mockDeleteOCSShare).toHaveBeenCalledWith('42')
        expect(mockDeleteNcShareId).toHaveBeenCalledWith('ABC123')
        // listShares called again after revoke
        expect(mockListShares).toHaveBeenCalledTimes(2)
    })
})
