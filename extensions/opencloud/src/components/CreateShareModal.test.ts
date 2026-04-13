import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import { setActivePinia, createPinia } from 'pinia'
import CreateShareModal from './CreateShareModal.vue'
import type { Resource } from '@opencloud-eu/web-client'

// Mock stores and services
const mockCreateLink = vi.fn()
const mockGetSpace = vi.fn()
const mockCreateShare = vi.fn()

vi.mock('@opencloud-eu/web-pkg', () => ({
  useSpacesStore: () => ({
    getSpace: mockGetSpace,
  }),
  useClientService: () => ({
    graphAuthenticated: {
      permissions: { createLink: mockCreateLink },
    },
  }),
}))
vi.mock('../composables/useAgentClient', () => ({
  useAgentClient: () => ({ createShare: mockCreateShare }),
}))

const mockResource: Resource = {
  id: '30e77cc8-3577-4c59-975a-166c5651f85d$abc!def',
  storageId: '30e77cc8-3577-4c59-975a-166c5651f85d',
  path: '/Documents/report.pdf',
  name: 'report.pdf',
}

describe('CreateShareModal', () => {
  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
    mockCreateLink.mockReset()
    mockCreateShare.mockReset()
    mockGetSpace.mockReset()
    mockGetSpace.mockReturnValue({ id: '30e77cc8-3577-4c59-975a-166c5651f85d' })
  })

  it('renders TTL selector with default 24h', () => {
    const wrapper = mount(CreateShareModal, { props: { resource: mockResource } })
    const select = wrapper.find('[data-testid="expiry-select"]')
    expect(select.exists()).toBe(true)
    expect((select.element as HTMLSelectElement).value).toBe('24')
  })

  it('renders password field', () => {
    const wrapper = mount(CreateShareModal, { props: { resource: mockResource } })
    expect(wrapper.find('[data-testid="password-input"]').exists()).toBe(true)
  })

  it('renders max downloads field with default 0', () => {
    const wrapper = mount(CreateShareModal, { props: { resource: mockResource } })
    const input = wrapper.find('[data-testid="max-downloads-input"]')
    expect(input.exists()).toBe(true)
    expect((input.element as HTMLInputElement).value).toBe('0')
  })

  it('emits close when Cancel is clicked', async () => {
    const wrapper = mount(CreateShareModal, { props: { resource: mockResource } })
    await wrapper.find('[data-testid="cancel-btn"]').trigger('click')
    expect(wrapper.emitted('close')).toBeTruthy()
  })

  it('calls Graph API then agent API on submit', async () => {
    const shareUrl = 'https://opencloud.example.com/s/XYZ789'
    const agentResult = { code: 'abc123', public_url: 'https://share.example.com/s/abc123', expires_at: '2026-04-10T12:00:00Z' }
    mockCreateLink.mockResolvedValue({ webUrl: shareUrl })
    mockCreateShare.mockResolvedValue(agentResult)

    const wrapper = mount(CreateShareModal, { props: { resource: mockResource } })
    await wrapper.find('[data-testid="create-btn"]').trigger('click')
    await wrapper.vm.$nextTick()
    await new Promise(resolve => setTimeout(resolve, 0))

    expect(mockGetSpace).toHaveBeenCalledWith(mockResource.storageId)
    expect(mockCreateLink).toHaveBeenCalledWith(
      '30e77cc8-3577-4c59-975a-166c5651f85d',
      mockResource.id,
      expect.objectContaining({ type: 'view' })
    )
    expect(mockCreateShare).toHaveBeenCalledWith(
      expect.objectContaining({ share_url: shareUrl })
    )
  })

  it('emits created event with agent result after successful create', async () => {
    const shareUrl = 'https://opencloud.example.com/s/XYZ789'
    const agentResult = { code: 'abc123', public_url: 'https://share.example.com/s/abc123', expires_at: '2026-04-10T12:00:00Z' }
    mockCreateLink.mockResolvedValue({ webUrl: shareUrl })
    mockCreateShare.mockResolvedValue(agentResult)

    const wrapper = mount(CreateShareModal, { props: { resource: mockResource } })
    await wrapper.find('[data-testid="create-btn"]').trigger('click')
    await new Promise(resolve => setTimeout(resolve, 0))

    expect(wrapper.emitted('created')).toBeTruthy()
    expect(wrapper.emitted('created')![0]).toEqual([agentResult])
  })

  it('shows error message when Graph share creation fails', async () => {
    mockCreateLink.mockRejectedValue(new Error('Graph error'))

    const wrapper = mount(CreateShareModal, { props: { resource: mockResource } })
    await wrapper.find('[data-testid="create-btn"]').trigger('click')
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="error-msg"]').text()).toContain(
      'Failed to create OpenCloud share'
    )
  })

  it('shows error message when agent share creation fails', async () => {
    mockCreateLink.mockResolvedValue({ webUrl: 'https://opencloud.example.com/s/XYZ' })
    mockCreateShare.mockRejectedValue(new Error('agent error'))

    const wrapper = mount(CreateShareModal, { props: { resource: mockResource } })
    await wrapper.find('[data-testid="create-btn"]').trigger('click')
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="error-msg"]').text()).toContain(
      'Failed to create ShareBridge share'
    )
  })

  it('shows TURN warning when relay-only checked and turnAvailable is false', async () => {
    const wrapper = mount(CreateShareModal, {
      props: { resource: mockResource, turnAvailable: false },
    })
    await wrapper.find('[data-testid="relay-only-input"]').setValue(true)
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[data-testid="turn-warning"]').exists()).toBe(true)
  })

  it('hides TURN warning when turnAvailable is true', async () => {
    const wrapper = mount(CreateShareModal, {
      props: { resource: mockResource, turnAvailable: true },
    })
    await wrapper.find('[data-testid="relay-only-input"]').setValue(true)
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[data-testid="turn-warning"]').exists()).toBe(false)
  })
})