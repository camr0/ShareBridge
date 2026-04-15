import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import ShareCard from './ShareCard.vue'
import type { Share } from '../types'

const makeShare = (overrides: Partial<Share> = {}): Share => ({
    code:          'ABC123',
    public_url:    'https://share.example.com/s/ABC123',
    share_url:     'https://nc.example.com/s/XYZ',
    file_id:       '12345',
    downloads:     2,
    max_downloads: 5,
    relay_only:    false,
    expires_at:    new Date(Date.now() + 3600 * 1000 * 50).toISOString(), // ~50 hours = clearly 2 days
    created_at:    new Date().toISOString(),
    ...overrides,
})

describe('ShareCard', () => {
    let mockWriteText: ReturnType<typeof vi.fn>

    beforeEach(() => {
        mockWriteText = vi.fn().mockResolvedValue(undefined)
        vi.stubGlobal('navigator', { clipboard: { writeText: mockWriteText } })
    })

    it('renders share code', () => {
        const wrapper = mount(ShareCard, { props: { share: makeShare() } })
        expect(wrapper.text()).toContain('ABC123')
    })

    it('renders download count', () => {
        const wrapper = mount(ShareCard, { props: { share: makeShare({ downloads: 2, max_downloads: 5 }) } })
        expect(wrapper.text()).toContain('2')
        expect(wrapper.text()).toContain('5')
    })

    it('shows ∞ when max_downloads is 0', () => {
        const wrapper = mount(ShareCard, { props: { share: makeShare({ max_downloads: 0 }) } })
        expect(wrapper.text()).toContain('∞')
    })

    it('shows "2 days" for a share expiring in ~48 hours', () => {
        const wrapper = mount(ShareCard, { props: { share: makeShare() } })
        expect(wrapper.text()).toContain('2 days')
    })

    it('shows "Expired" for a share past its expiry', () => {
        const wrapper = mount(ShareCard, {
            props: { share: makeShare({ expires_at: new Date(Date.now() - 1000).toISOString() }) },
        })
        expect(wrapper.text()).toContain('Expired')
    })

    it('emits revoke event with code when Revoke is clicked', async () => {
        const wrapper = mount(ShareCard, { props: { share: makeShare() } })
        await wrapper.find('[data-testid="revoke-btn"]').trigger('click')
        expect(wrapper.emitted('revoke')).toBeTruthy()
        expect(wrapper.emitted('revoke')![0]).toEqual(['ABC123'])
    })

    it('copies public_url to clipboard when Copy Link is clicked', async () => {
        const wrapper = mount(ShareCard, { props: { share: makeShare() } })
        await wrapper.find('[data-testid="copy-btn"]').trigger('click')
        expect(mockWriteText).toHaveBeenCalledWith('https://share.example.com/s/ABC123')
    })
})