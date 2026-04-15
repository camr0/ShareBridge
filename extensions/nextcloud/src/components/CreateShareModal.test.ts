import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { setActivePinia, createPinia } from 'pinia'
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
        const select = wrapper.find('[data-testid="expiry-select"]')
        expect(select.exists()).toBe(true)
        const options = select.findAll('option')
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

    it('renders relay-only checkbox', () => {
        const wrapper = mountModal()
        expect(wrapper.find('[data-testid="relay-only-input"]').exists()).toBe(true)
    })

    it('shows TURN warning when relay is checked and TURN is not available', async () => {
        const wrapper = mountModal({ turnAvailable: false })
        const relayCheckbox = wrapper.find('[data-testid="relay-only-input"]')
        await relayCheckbox.trigger('change')
        await wrapper.vm.$nextTick()
        expect(wrapper.find('[data-testid="turn-warning"]').exists()).toBe(true)
    })

    it('does not show TURN warning when relay is checked but TURN is available', async () => {
        const wrapper = mountModal({ turnAvailable: true })
        const relayCheckbox = wrapper.find('[data-testid="relay-only-input"]')
        await relayCheckbox.trigger('change')
        await wrapper.vm.$nextTick()
        expect(wrapper.find('[data-testid="turn-warning"]').exists()).toBe(false)
    })

    it('emits close when Cancel is clicked', async () => {
        const wrapper = mountModal()
        await wrapper.find('[data-testid="cancel-btn"]').trigger('click')
        expect(wrapper.emitted('close')).toBeTruthy()
    })

    it('on submit: calls createOCSShare, createShare, saveNcShareId, then emits created', async () => {
        const wrapper = mountModal()
        await wrapper.find('[data-testid="create-btn"]').trigger('click')
        await flushPromises()

        expect(mockCreateOCSShare).toHaveBeenCalledWith('/Documents/report.pdf', expect.any(Number))
        expect(mockCreateShare).toHaveBeenCalledWith(expect.objectContaining({
            share_url: 'https://nc.example.com/s/XYZ',
        }))
        expect(mockSaveNcShareId).toHaveBeenCalledWith('ABC123', '42')
        expect(wrapper.emitted('created')).toBeTruthy()
    })

    it('shows error when OCS share creation fails with PASSWORD_REQUIRED', async () => {
        mockCreateOCSShare.mockRejectedValueOnce(new Error('PASSWORD_REQUIRED'))

        const wrapper = mountModal()
        await wrapper.find('[data-testid="create-btn"]').trigger('click')
        await flushPromises()

        expect(wrapper.find('[data-testid="error-msg"]').text()).toContain('password')
    })

    it('shows loading state while creating share', async () => {
        let resolveCreate: () => void
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
        resolveCreate!()
        await flushPromises()

        // Loading state should be cleared
        expect(createBtn.text()).toContain('Create Share')
    })
})