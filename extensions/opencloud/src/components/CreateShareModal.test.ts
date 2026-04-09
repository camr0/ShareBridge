import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import { setActivePinia, createPinia } from 'pinia'
import CreateShareModal from './CreateShareModal.vue'

// Mock composables
const mockCreatePublicShare = vi.fn()
const mockCreateShare = vi.fn()

vi.mock('../composables/useOpenCloudAPI', () => ({
  useOpenCloudAPI: () => ({ createPublicShare: mockCreatePublicShare }),
}))
vi.mock('../composables/useAgentClient', () => ({
  useAgentClient: () => ({ createShare: mockCreateShare }),
}))

describe('CreateShareModal', () => {
  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
    mockCreatePublicShare.mockReset()
    mockCreateShare.mockReset()
  })

  it('renders TTL selector with default 24h', () => {
    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    const select = wrapper.find('[data-testid="expiry-select"]')
    expect(select.exists()).toBe(true)
    expect((select.element as HTMLSelectElement).value).toBe('24')
  })

  it('renders password field', () => {
    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    expect(wrapper.find('[data-testid="password-input"]').exists()).toBe(true)
  })

  it('renders max downloads field with default 0', () => {
    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    const input = wrapper.find('[data-testid="max-downloads-input"]')
    expect(input.exists()).toBe(true)
    expect((input.element as HTMLInputElement).value).toBe('0')
  })

  it('emits close when Cancel is clicked', async () => {
    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    await wrapper.find('[data-testid="cancel-btn"]').trigger('click')
    expect(wrapper.emitted('close')).toBeTruthy()
  })

  it('calls OCS API then agent API on submit', async () => {
    const shareUrl = 'https://opencloud.example.com/s/XYZ789'
    const agentResult = { code: 'abc123', public_url: 'https://share.example.com/s/abc123', expires_at: '2026-04-10T12:00:00Z' }
    mockCreatePublicShare.mockResolvedValue(shareUrl)
    mockCreateShare.mockResolvedValue(agentResult)

    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    await wrapper.find('[data-testid="create-btn"]').trigger('click')
    await wrapper.vm.$nextTick()
    // Wait for async operations
    await new Promise(resolve => setTimeout(resolve, 0))

    expect(mockCreatePublicShare).toHaveBeenCalledWith(
      '/file.pdf',
      expect.objectContaining({})
    )
    expect(mockCreateShare).toHaveBeenCalledWith(
      expect.objectContaining({ share_url: shareUrl })
    )
  })

  it('emits created event with agent result after successful create', async () => {
    const shareUrl = 'https://opencloud.example.com/s/XYZ789'
    const agentResult = { code: 'abc123', public_url: 'https://share.example.com/s/abc123', expires_at: '2026-04-10T12:00:00Z' }
    mockCreatePublicShare.mockResolvedValue(shareUrl)
    mockCreateShare.mockResolvedValue(agentResult)

    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    await wrapper.find('[data-testid="create-btn"]').trigger('click')
    await new Promise(resolve => setTimeout(resolve, 0))

    expect(wrapper.emitted('created')).toBeTruthy()
    expect(wrapper.emitted('created')![0]).toEqual([agentResult])
  })

  it('shows error message when OCS share creation fails', async () => {
    mockCreatePublicShare.mockRejectedValue(new Error('OCS error'))

    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    await wrapper.find('[data-testid="create-btn"]').trigger('click')
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="error-msg"]').text()).toContain(
      'Failed to create OpenCloud share'
    )
  })

  it('shows error message when agent share creation fails', async () => {
    mockCreatePublicShare.mockResolvedValue('https://opencloud.example.com/s/XYZ')
    mockCreateShare.mockRejectedValue(new Error('agent error'))

    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    await wrapper.find('[data-testid="create-btn"]').trigger('click')
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="error-msg"]').text()).toContain(
      'Failed to create ShareBridge share'
    )
  })

  it('shows TURN warning when relay-only checked and turnAvailable is false', async () => {
    const wrapper = mount(CreateShareModal, {
      props: { filePath: '/file.pdf', turnAvailable: false },
    })
    await wrapper.find('[data-testid="relay-only-input"]').setValue(true)
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[data-testid="turn-warning"]').exists()).toBe(true)
  })

  it('hides TURN warning when turnAvailable is true', async () => {
    const wrapper = mount(CreateShareModal, {
      props: { filePath: '/file.pdf', turnAvailable: true },
    })
    await wrapper.find('[data-testid="relay-only-input"]').setValue(true)
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[data-testid="turn-warning"]').exists()).toBe(false)
  })
})