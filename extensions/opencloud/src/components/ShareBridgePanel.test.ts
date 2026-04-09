import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import { setActivePinia, createPinia } from 'pinia'
import ShareBridgePanel from './ShareBridgePanel.vue'
import type { Share } from '../types'

const mockListShares = vi.fn()
const mockRevokeShare = vi.fn()
const mockGetSettings = vi.fn()

vi.mock('../composables/useAgentClient', () => ({
  useAgentClient: () => ({
    listShares: mockListShares,
    revokeShare: mockRevokeShare,
    getSettings: mockGetSettings,  // called in onMounted for turnAvailable
  }),
}))
vi.mock('./ShareCard.vue', () => ({ default: { template: '<div data-testid="share-card">{{ share.code }}</div>', props: ['share'] } }))
vi.mock('./CreateShareModal.vue', () => ({ default: { name: 'CreateShareModal', template: '<div data-testid="create-modal" />', props: ['filePath'], emits: ['close', 'created'] } }))

const makeShare = (code = 'abc123'): Share => ({
  code,
  public_url: `https://share.example.com/s/${code}`,
  share_url: 'https://opencloud.example.com/s/XYZ',
  file_id: 'storage-users-1$abc!def',
  downloads: 0,
  max_downloads: 10,
  relay_only: false,
  expires_at: '2026-04-10T12:00:00Z',
  created_at: '2026-04-09T12:00:00Z',
})

const defaultProps = {
  resource: {
    id: 'storage-users-1$abc!def',
    path: '/Documents/report.pdf',
    name: 'report.pdf',
  },
}

describe('ShareBridgePanel', () => {
  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
    mockListShares.mockReset()
    mockRevokeShare.mockReset()
    mockGetSettings.mockReset()
    mockGetSettings.mockResolvedValue({
      default_expiry_hours: 24,
      default_max_downloads: 0,
      default_relay_only: false,
      turn_available: true,
    })
  })

  it('shows configure prompt when agent not configured', async () => {
    // Settings store has empty agentUrl and apiKey by default
    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[data-testid="configure-prompt"]').exists()).toBe(true)
  })

  it('renders agent URL and API key inputs in configure prompt', async () => {
    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[data-testid="agent-url-input"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="api-key-input"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="save-config-btn"]').exists()).toBe(true)
  })

  it('save button is disabled when fields are empty', async () => {
    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await wrapper.vm.$nextTick()
    const saveBtn = wrapper.find('[data-testid="save-config-btn"]')
    expect((saveBtn.element as HTMLButtonElement).disabled).toBe(true)
  })

  it('save button is enabled when both fields are filled', async () => {
    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await wrapper.vm.$nextTick()
    await wrapper.find('[data-testid="agent-url-input"]').setValue('http://localhost:7878')
    await wrapper.find('[data-testid="api-key-input"]').setValue('sb_agent_test')
    const saveBtn = wrapper.find('[data-testid="save-config-btn"]')
    expect((saveBtn.element as HTMLButtonElement).disabled).toBe(false)
  })

  it('save config persists to localStorage and loads shares', async () => {
    mockListShares.mockResolvedValue([makeShare('abc123')])
    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await wrapper.vm.$nextTick()
    await wrapper.find('[data-testid="agent-url-input"]').setValue('http://localhost:7878')
    await wrapper.find('[data-testid="api-key-input"]').setValue('sb_agent_test')
    await wrapper.find('[data-testid="save-config-btn"]').trigger('click')
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(localStorage.getItem('sharebridge_agent_url')).toBe('http://localhost:7878')
    expect(localStorage.getItem('sharebridge_api_key')).toBe('sb_agent_test')
    expect(wrapper.find('[data-testid="share-list"]').exists()).toBe(true)
  })

  it('shows share list when configured', async () => {
    localStorage.setItem('sharebridge_agent_url', 'http://localhost:7878')
    localStorage.setItem('sharebridge_api_key', 'sb_agent_key')
    mockListShares.mockResolvedValue([makeShare('abc123')])

    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="share-list"]').exists()).toBe(true)
  })

  it('shows empty state when no shares exist for file', async () => {
    localStorage.setItem('sharebridge_agent_url', 'http://localhost:7878')
    localStorage.setItem('sharebridge_api_key', 'sb_agent_key')
    mockListShares.mockResolvedValue([])

    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="empty-state"]').exists()).toBe(true)
  })

  it('shows create share button when configured', async () => {
    localStorage.setItem('sharebridge_agent_url', 'http://localhost:7878')
    localStorage.setItem('sharebridge_api_key', 'sb_agent_key')
    mockListShares.mockResolvedValue([])

    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="create-share-btn"]').exists()).toBe(true)
  })

  it('opens modal when Create Share button is clicked', async () => {
    localStorage.setItem('sharebridge_agent_url', 'http://localhost:7878')
    localStorage.setItem('sharebridge_api_key', 'sb_agent_key')
    mockListShares.mockResolvedValue([])

    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    await wrapper.find('[data-testid="create-share-btn"]').trigger('click')
    expect(wrapper.find('[data-testid="create-modal"]').exists()).toBe(true)
  })

  it('refreshes share list after new share is created', async () => {
    localStorage.setItem('sharebridge_agent_url', 'http://localhost:7878')
    localStorage.setItem('sharebridge_api_key', 'sb_agent_key')
    mockListShares
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce([makeShare('new123')])

    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    // Simulate modal created event
    await wrapper.find('[data-testid="create-share-btn"]').trigger('click')
    await wrapper.vm.$nextTick()
    // Trigger 'created' on the modal
    const modal = wrapper.findComponent({ name: 'CreateShareModal' })
    await modal.vm.$emit('created', { code: 'new123', public_url: 'https://share.example.com/s/new123', expires_at: '2026-04-10T12:00:00Z' })
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    // listShares called twice: initial load + after create
    expect(mockListShares).toHaveBeenCalledTimes(2)
  })

  it('shows error message when agent is unreachable', async () => {
    localStorage.setItem('sharebridge_agent_url', 'http://localhost:7878')
    localStorage.setItem('sharebridge_api_key', 'sb_agent_key')
    mockListShares.mockRejectedValue(new Error('Network error'))

    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="error-msg"]').text()).toContain(
      'Cannot connect to ShareBridge agent'
    )
  })
})