import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { mount } from '@vue/test-utils'
import { setActivePinia, createPinia } from 'pinia'
import CreateShareModal from './CreateShareModal.vue'
import type { Resource } from '@opencloud-eu/web-client'

// Mock stores and services
const mockCreateLink = vi.fn()
const mockCreateShare = vi.fn()

vi.mock('@opencloud-eu/web-pkg', () => ({
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
  })

  afterEach(() => {
    // Clean up teleported elements from document.body
    document.body.innerHTML = ''
  })

  // Helper to mount with Teleport support - query from document.body
  const mountModal = (props: Record<string, unknown> = {}) =>
    mount(CreateShareModal, {
      props: { resource: mockResource, ...props },
      attachTo: document.body,
    })

  // Helper to find teleported elements
  const findEl = (testId: string) => document.querySelector(`[data-testid="${testId}"]`)
  const findEls = (testId: string) => document.querySelectorAll(`[data-testid="${testId}"]`)

  it('renders TTL selector with default from props', () => {
    const wrapper = mountModal({ defaultExpiryHours: 168 })
    const select = findEl('expiry-select') as HTMLSelectElement
    expect(select).toBeTruthy()
    expect(select?.value).toBe('168')
  })

  it('falls back to 24h expiry when no default prop provided', () => {
    const wrapper = mountModal()
    const select = findEl('expiry-select') as HTMLSelectElement
    expect(select?.value).toBe('24')
  })

  it('renders password field', () => {
    const wrapper = mountModal()
    expect(findEl('password-input')).toBeTruthy()
  })

  it('renders max downloads field with default from props', () => {
    const wrapper = mountModal({ defaultMaxDownloads: 10 })
    const input = findEl('max-downloads-input') as HTMLInputElement
    expect(input).toBeTruthy()
    expect(input?.value).toBe('10')
  })

  it('falls back to 0 max downloads when no default prop provided', () => {
    const wrapper = mountModal()
    const input = findEl('max-downloads-input') as HTMLInputElement
    expect(input?.value).toBe('0')
  })

  it('emits close when Cancel is clicked', async () => {
    const wrapper = mountModal()
    const btn = findEl('cancel-btn') as HTMLButtonElement
    btn?.click()
    await wrapper.vm.$nextTick()
    expect(wrapper.emitted('close')).toBeTruthy()
  })

  it('calls Graph API then agent API on submit', async () => {
    const shareUrl = 'https://opencloud.example.com/s/XYZ789'
    const agentResult = { code: 'abc123', public_url: 'https://share.example.com/s/abc123', expires_at: '2026-04-10T12:00:00Z' }
    mockCreateLink.mockResolvedValue({ webUrl: shareUrl })
    mockCreateShare.mockResolvedValue(agentResult)

    const wrapper = mountModal()
    const btn = findEl('create-btn') as HTMLButtonElement
    btn?.click()
    await wrapper.vm.$nextTick()
    await new Promise(resolve => setTimeout(resolve, 0))

    expect(mockCreateLink).toHaveBeenCalledWith(
      mockResource.storageId,
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

    const wrapper = mountModal()
    const btn = findEl('create-btn') as HTMLButtonElement
    btn?.click()
    await new Promise(resolve => setTimeout(resolve, 0))

    expect(wrapper.emitted('created')).toBeTruthy()
    expect(wrapper.emitted('created')![0]).toEqual([agentResult])
  })

  it('shows error message when Graph share creation fails', async () => {
    mockCreateLink.mockRejectedValue(new Error('Graph error'))

    const wrapper = mountModal()
    const btn = findEl('create-btn') as HTMLButtonElement
    btn?.click()
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    const errorMsg = findEl('error-msg')
    expect(errorMsg?.textContent).toContain('Failed to create OpenCloud share')
  })

  it('shows error message when agent share creation fails', async () => {
    mockCreateLink.mockResolvedValue({ webUrl: 'https://opencloud.example.com/s/XYZ' })
    mockCreateShare.mockRejectedValue(new Error('agent error'))

    const wrapper = mountModal()
    const btn = findEl('create-btn') as HTMLButtonElement
    btn?.click()
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    const errorMsg = findEl('error-msg')
    expect(errorMsg?.textContent).toContain('Failed to create ShareBridge share')
  })

  it('shows TURN warning when relay-only checked and turnAvailable is false', async () => {
    const wrapper = mountModal({ turnAvailable: false })
    const checkbox = findEl('relay-only-input') as HTMLInputElement
    checkbox?.click()
    await wrapper.vm.$nextTick()
    expect(findEl('turn-warning')).toBeTruthy()
  })

  it('hides TURN warning when turnAvailable is true', async () => {
    const wrapper = mountModal({ turnAvailable: true })
    const checkbox = findEl('relay-only-input') as HTMLInputElement
    checkbox?.click()
    await wrapper.vm.$nextTick()
    expect(findEl('turn-warning')).toBeFalsy()
  })

  it('renders relay-only checkbox with default from props', () => {
    const wrapper = mountModal({ defaultRelayOnly: true })
    const checkbox = findEl('relay-only-input') as HTMLInputElement
    expect(checkbox?.checked).toBe(true)
  })

  it('falls back to relay-only unchecked when no default prop provided', () => {
    const wrapper = mountModal()
    const checkbox = findEl('relay-only-input') as HTMLInputElement
    expect(checkbox?.checked).toBe(false)
  })

  it('includes custom expiry option when default is not a preset', async () => {
    const wrapper = mountModal({ defaultExpiryHours: 48 })
    await wrapper.vm.$nextTick()
    const select = findEl('expiry-select')
    const options = select?.querySelectorAll('option')
    const values = Array.from(options || []).map(o => parseInt(o.getAttribute('value') || ''))
    expect(values).toContain(48)
  })
})