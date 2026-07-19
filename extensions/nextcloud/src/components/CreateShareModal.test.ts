import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { setActivePinia, createPinia } from 'pinia'
import { NcNoteCard } from '@nextcloud/vue'
import { useSettingsStore } from '../stores/settings'
import CreateShareModal from './CreateShareModal.vue'

// Create mutable mock functions that can be controlled in tests
const mockCreateOCSShare = vi.fn().mockResolvedValue({ shareUrl: 'https://nc.example.com/s/XYZ', shareId: '42' })
const mockSaveNcShareId = vi.fn().mockResolvedValue(undefined)
const mockCreateShare = vi.fn().mockResolvedValue({ code: 'ABC123', public_url: 'https://share.example.com/s/ABC123', expires_at: '2026-04-15T00:00:00Z' })

// Mock composables so tests don't hit real network
vi.mock('../composables/useNextcloudOCS', () => ({
    useNextcloudOCS: () => ({
        createOCSShare: mockCreateOCSShare,
        saveNcShareId: mockSaveNcShareId,
    }),
}))
vi.mock('../composables/useAgentClient', () => ({
    useAgentClient: () => ({
        createShare: mockCreateShare,
    }),
}))

describe('CreateShareModal', () => {
    beforeEach(() => {
        setActivePinia(createPinia())
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })
        // Reset mock call counts
        mockCreateOCSShare.mockClear()
        mockSaveNcShareId.mockClear()
        mockCreateShare.mockClear()
        // Reset to default implementations
        mockCreateOCSShare.mockResolvedValue({ shareUrl: 'https://nc.example.com/s/XYZ', shareId: '42' })
        mockCreateShare.mockResolvedValue({ code: 'ABC123', public_url: 'https://share.example.com/s/ABC123', expires_at: '2026-04-15T00:00:00Z' })
    })

    const mountModal = (props = {}) => mount(CreateShareModal, {
        props: { filePath: '/Documents/report.pdf', turnAvailable: true, ...props },
    })

    it('renders expiry select with preset options', () => {
        const wrapper = mountModal()
        const selectWrapper = wrapper.find('[data-testid="expiry-select"]')
        expect(selectWrapper.exists()).toBe(true)
        const options = selectWrapper.findAll('option')
        expect(options.length).toBeGreaterThan(0)
    })

    it('renders password input', () => {
        const wrapper = mountModal()
        expect(wrapper.find('[data-testid="password-input"]').exists()).toBe(true)
    })

    it('renders max-downloads input', () => {
        const wrapper = mountModal()
        expect(wrapper.find('[data-testid="max-downloads-input"]').exists()).toBe(true)
    })

    it('renders Relay first and Direct second with Relay selected by default', () => {
        const wrapper = mountModal()

        const options = wrapper.findAll('[data-testid$="-option"]')
        expect(options).toHaveLength(2)
        expect(options[0].attributes('data-testid')).toBe('relay-option')
        expect(options[1].attributes('data-testid')).toBe('direct-option')

        const relay = wrapper.find<HTMLInputElement>('[data-testid="mode-relay"]')
        const direct = wrapper.find<HTMLInputElement>('[data-testid="mode-direct"]')
        expect(relay.attributes('type')).toBe('radio')
        expect(direct.attributes('type')).toBe('radio')
        expect(relay.element.checked).toBe(true)
        expect(direct.element.checked).toBe(false)
        expect(relay.attributes('name')).toBe(direct.attributes('name'))
        expect(relay.attributes('name')).toBeTruthy()
        expect(options[0].text()).toContain('Relay (recommended)')
        expect(options[0].text()).toContain('End-to-end encrypted, hides your IP, and provides consistent performance')
        expect(options[1].text()).toContain('Direct')
        expect(options[1].text()).toContain('Peer-to-peer, quota-free')
    })

    it('does not render TURN availability copy', () => {
        const wrapper = mountModal({ turnAvailable: false })
        expect(wrapper.text()).not.toContain('TURN')
    })

    it('shows the Direct advisory only after Direct is selected', async () => {
        const wrapper = mountModal()
        expect(wrapper.find('[data-testid="direct-advisory"]').exists()).toBe(false)

        await wrapper.find<HTMLInputElement>('[data-testid="mode-direct"]').setValue(true)

        expect(wrapper.find<HTMLInputElement>('[data-testid="mode-relay"]').element.checked).toBe(false)
        expect(wrapper.find<HTMLInputElement>('[data-testid="mode-direct"]').element.checked).toBe(true)
        expect(wrapper.find('[data-testid="direct-advisory"]').text()).toBe(
            'Direct transfers expose your IP address and may be slower due to browser protocol limitations. Use Relay for more consistent performance.',
        )
        expect(wrapper.findComponent(NcNoteCard).props('type')).toBe('warning')
    })

    it('uses distinct radio IDs and groups for multiple modal instances', async () => {
        const first = mountModal()
        const second = mountModal()

        const firstRelay = first.find<HTMLInputElement>('[data-testid="mode-relay"]')
        const firstDirect = first.find<HTMLInputElement>('[data-testid="mode-direct"]')
        const secondRelay = second.find<HTMLInputElement>('[data-testid="mode-relay"]')
        const secondDirect = second.find<HTMLInputElement>('[data-testid="mode-direct"]')

        expect(firstRelay.attributes('id')).not.toBe(secondRelay.attributes('id'))
        expect(firstDirect.attributes('id')).not.toBe(secondDirect.attributes('id'))
        expect(first.find('[data-testid="relay-option"]').attributes('for')).toBe(firstRelay.attributes('id'))
        expect(first.find('[data-testid="direct-option"]').attributes('for')).toBe(firstDirect.attributes('id'))
        expect(second.find('[data-testid="relay-option"]').attributes('for')).toBe(secondRelay.attributes('id'))
        expect(second.find('[data-testid="direct-option"]').attributes('for')).toBe(secondDirect.attributes('id'))
        expect(firstRelay.attributes('name')).not.toBe(secondRelay.attributes('name'))

        await firstDirect.setValue(true)
        expect(firstRelay.element.checked).toBe(false)
        expect(firstDirect.element.checked).toBe(true)
        expect(secondRelay.element.checked).toBe(true)
        expect(secondDirect.element.checked).toBe(false)
    })

    it('selects Direct when defaultRelayOnly is explicitly false', () => {
        const wrapper = mountModal({ defaultRelayOnly: false })
        expect(wrapper.find<HTMLInputElement>('[data-testid="mode-relay"]').element.checked).toBe(false)
        expect(wrapper.find<HTMLInputElement>('[data-testid="mode-direct"]').element.checked).toBe(true)
    })

    it('emits close when Cancel is clicked', async () => {
        const wrapper = mountModal()
        await wrapper.find('[data-testid="cancel-btn"]').trigger('click')
        expect(wrapper.emitted('close')).toBeTruthy()
    })

    it('on submit: defaults to relay-only and completes share creation', async () => {
        const wrapper = mountModal()
        await wrapper.find('[data-testid="create-btn"]').trigger('click')
        await flushPromises()

        expect(mockCreateOCSShare).toHaveBeenCalledWith('/Documents/report.pdf', expect.any(Number))
        expect(mockCreateShare).toHaveBeenCalledWith(expect.objectContaining({
            share_url: 'https://nc.example.com/s/XYZ',
            relay_only: true,
        }))
        expect(mockSaveNcShareId).toHaveBeenCalledWith('ABC123', '42')
        expect(wrapper.emitted('created')).toBeTruthy()
    })

    it('on submit: sends relay_only false after Direct is selected', async () => {
        const wrapper = mountModal()
        await wrapper.find<HTMLInputElement>('[data-testid="mode-direct"]').setValue(true)
        await wrapper.find('[data-testid="create-btn"]').trigger('click')
        await flushPromises()

        expect(mockCreateShare).toHaveBeenCalledWith(expect.objectContaining({
            relay_only: false,
        }))
    })

    it('shows error when OCS share creation fails with PASSWORD_REQUIRED', async () => {
        mockCreateOCSShare.mockRejectedValueOnce(new Error('PASSWORD_REQUIRED'))

        const wrapper = mountModal()
        await wrapper.find('[data-testid="create-btn"]').trigger('click')
        await flushPromises()

        expect(wrapper.find('[data-testid="error-msg"]').text()).toContain('password')
    })

    it('shows loading state while creating share', async () => {
        let resolveCreate: (value: unknown) => void
        mockCreateOCSShare.mockImplementation(() => new Promise(resolve => { resolveCreate = resolve }))

        const wrapper = mountModal()
        const createBtn = wrapper.find('[data-testid="create-btn"]')

        // Click create button
        await createBtn.trigger('click')
        await wrapper.vm.$nextTick()

        // Check loading state
        expect(createBtn.text()).toContain('Creating')
        expect(createBtn.attributes('disabled')).toBeDefined()

        // Resolve the promise
        resolveCreate!(undefined)
        await flushPromises()

        // Loading state should be cleared
        expect(createBtn.text()).toContain('Create Share')
    })
})
