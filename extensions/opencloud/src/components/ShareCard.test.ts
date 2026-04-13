import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import ShareCard from './ShareCard.vue'
import type { Share } from '../types'

const makeShare = (overrides: Partial<Share> = {}): Share => ({
  code: 'abc123',
  public_url: 'https://share.example.com/s/abc123',
  share_url: 'https://opencloud.example.com/s/XYZ',
  file_id: 'storage-users-1$abc!def',
  downloads: 3,
  max_downloads: 10,
  relay_only: false,
  expires_at: '2026-04-10T12:00:00Z',
  created_at: '2026-04-09T12:00:00Z',
  ...overrides,
})

describe('ShareCard', () => {
  let mockWriteText: ReturnType<typeof vi.fn>

  beforeEach(() => {
    mockWriteText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', {
      clipboard: {
        writeText: mockWriteText,
      },
    })
  })

  it('renders share code', () => {
    const wrapper = mount(ShareCard, { props: { share: makeShare() } })
    expect(wrapper.text()).toContain('abc123')
  })

  it('renders download count', () => {
    const wrapper = mount(ShareCard, { props: { share: makeShare({ downloads: 3, max_downloads: 10 }) } })
    expect(wrapper.text()).toContain('3')
    expect(wrapper.text()).toContain('10')
  })

  it('shows ∞ when max_downloads is 0', () => {
    const wrapper = mount(ShareCard, { props: { share: makeShare({ max_downloads: 0 }) } })
    expect(wrapper.text()).toContain('∞')
  })

  it('emits revoke event with code when Revoke button is clicked', async () => {
    const wrapper = mount(ShareCard, { props: { share: makeShare() } })
    await wrapper.find('[data-testid="revoke-btn"]').trigger('click')
    expect(wrapper.emitted('revoke')).toBeTruthy()
    expect(wrapper.emitted('revoke')![0]).toEqual(['abc123'])
  })

  it('copy button copies public_url to clipboard', async () => {
    const wrapper = mount(ShareCard, { props: { share: makeShare() } })
    await wrapper.find('[data-testid="copy-btn"]').trigger('click')

    expect(mockWriteText).toHaveBeenCalledWith('https://share.example.com/s/abc123')
  })

  it('shows copied feedback after copying', async () => {
    vi.useFakeTimers()

    const wrapper = mount(ShareCard, { props: { share: makeShare() } })
    await wrapper.find('[data-testid="copy-btn"]').trigger('click')
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="copied-feedback"]').exists()).toBe(true)

    vi.advanceTimersByTime(2001)
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[data-testid="copied-feedback"]').exists()).toBe(false)

    vi.useRealTimers()
  })
})