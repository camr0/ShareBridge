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

  it('renders TTL selector with default from props', () => {
    mountModal({ defaultExpiryHours: 168 })
    const select = findEl('expiry-select') as HTMLSelectElement
    expect(select).toBeTruthy()
    expect(select?.value).toBe('168')
  })

  it('falls back to 24h expiry when no default prop provided', () => {
    mountModal()
    const select = findEl('expiry-select') as HTMLSelectElement
    expect(select?.value).toBe('24')
  })

  it('renders password field', () => {
    mountModal()
    expect(findEl('password-input')).toBeTruthy()
  })

  it('renders max downloads field with default from props', () => {
    mountModal({ defaultMaxDownloads: 10 })
    const input = findEl('max-downloads-input') as HTMLInputElement
    expect(input).toBeTruthy()
    expect(input?.value).toBe('10')
  })

  it('falls back to 0 max downloads when no default prop provided', () => {
    mountModal()
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
      expect.objectContaining({ share_url: shareUrl, relay_only: true })
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

  it('renders explicit Relay and Direct choices in recommended order without TURN copy', () => {
    const wrapper = mountModal()
    const options = Array.from(document.querySelectorAll('[data-testid$="-option"]'))

    expect(options.map(option => option.getAttribute('data-testid'))).toEqual([
      'relay-option',
      'direct-option',
    ])
    expect(findEl('relay-option')?.textContent).toContain('Relay (recommended)')
    expect(findEl('relay-option')?.querySelector('span span')?.textContent?.trim()).toBe(
      'End-to-end encrypted, hides your IP, and provides consistent performance.'
    )
    expect(findEl('direct-option')?.textContent).toContain('Direct')
    expect(findEl('direct-option')?.querySelector('span span')?.textContent?.trim()).toBe(
      'Peer-to-peer, quota-free.'
    )
    expect(document.body.textContent).not.toContain('TURN')
    wrapper.unmount()
  })

  it('selects Relay when defaultRelayOnly is omitted', () => {
    const wrapper = mountModal()
    expect((findEl('mode-relay') as HTMLInputElement)?.checked).toBe(true)
    expect((findEl('mode-direct') as HTMLInputElement)?.checked).toBe(false)
    wrapper.unmount()
  })

  it('shows the Direct advisory only when Direct is selected', async () => {
    const wrapper = mountModal()
    expect(findEl('direct-advisory')).toBeFalsy()

    const directRadio = findEl('mode-direct') as HTMLInputElement
    directRadio.click()
    await wrapper.vm.$nextTick()

    expect(findEl('direct-advisory')?.textContent?.trim()).toBe(
      'Direct transfers expose your IP address and may be slower due to browser protocol limitations. Use Relay for more consistent performance.'
    )
  })

  it('submits relay_only false after Direct is selected', async () => {
    const shareUrl = 'https://opencloud.example.com/s/DIRECT'
    mockCreateLink.mockResolvedValue({ webUrl: shareUrl })
    mockCreateShare.mockResolvedValue({
      code: 'direct123',
      public_url: 'https://share.example.com/s/direct123',
      expires_at: '2026-04-10T12:00:00Z',
    })

    const wrapper = mountModal()
    const directRadio = findEl('mode-direct') as HTMLInputElement
    const createButton = findEl('create-btn') as HTMLButtonElement
    directRadio.click()
    createButton.click()
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(mockCreateShare).toHaveBeenCalledWith(
      expect.objectContaining({ share_url: shareUrl, relay_only: false })
    )
  })

  it('keeps connection mode selections independent across modal instances', async () => {
    const firstWrapper = mountModal()
    mountModal()
    const relayRadios = document.querySelectorAll<HTMLInputElement>('[data-testid="mode-relay"]')
    const directRadios = document.querySelectorAll<HTMLInputElement>('[data-testid="mode-direct"]')

    expect(Array.from(relayRadios, radio => radio.checked)).toEqual([true, true])

    directRadios[0].click()
    await firstWrapper.vm.$nextTick()

    expect(directRadios[0].checked).toBe(true)
    expect(relayRadios[1].checked).toBe(true)
  })

  it('uses a shared native radio group name within each modal and a unique name across modals', () => {
    mountModal()
    mountModal()
    const relayRadios = document.querySelectorAll<HTMLInputElement>('[data-testid="mode-relay"]')
    const directRadios = document.querySelectorAll<HTMLInputElement>('[data-testid="mode-direct"]')

    expect(relayRadios[0].name).not.toBe('')
    expect(relayRadios[0].name).toBe(directRadios[0].name)
    expect(relayRadios[1].name).toBe(directRadios[1].name)
    expect(relayRadios[0].name).not.toBe(relayRadios[1].name)
  })

  it('selects Direct when defaultRelayOnly is explicitly false', () => {
    mountModal({ defaultRelayOnly: false })
    expect((findEl('mode-relay') as HTMLInputElement)?.checked).toBe(false)
    expect((findEl('mode-direct') as HTMLInputElement)?.checked).toBe(true)
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
