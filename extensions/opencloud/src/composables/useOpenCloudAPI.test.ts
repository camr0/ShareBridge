import { describe, it, expect, vi, beforeEach } from 'vitest'
import { useOpenCloudAPI } from './useOpenCloudAPI'

// Mock @opencloud-eu/web-pkg
const mockOcsPost = vi.fn()
vi.mock('@opencloud-eu/web-pkg', () => ({
  useClientService: () => ({
    httpAuthenticated: { post: mockOcsPost },
  }),
}))

describe('useOpenCloudAPI', () => {
  beforeEach(() => {
    mockOcsPost.mockReset()
  })

  it('createPublicShare POSTs to OCS shares endpoint with shareType=3 and path', async () => {
    mockOcsPost.mockResolvedValue({
      data: { ocs: { data: { url: 'https://opencloud.example.com/s/XYZ789' } } },
    })

    const { createPublicShare } = useOpenCloudAPI()
    const result = await createPublicShare('/Documents/report.pdf')

    expect(mockOcsPost).toHaveBeenCalledWith(
      '/apps/files_sharing/api/v1/shares',
      expect.any(URLSearchParams)
    )
    const params = mockOcsPost.mock.calls[0][1] as URLSearchParams
    expect(params.get('shareType')).toBe('3')
    expect(params.get('path')).toBe('/Documents/report.pdf')
    expect(result).toBe('https://opencloud.example.com/s/XYZ789')
  })

  it('createPublicShare includes password when provided', async () => {
    mockOcsPost.mockResolvedValue({
      data: { ocs: { data: { url: 'https://opencloud.example.com/s/ABC' } } },
    })

    const { createPublicShare } = useOpenCloudAPI()
    await createPublicShare('/file.pdf', { password: 'secret' })

    const params = mockOcsPost.mock.calls[0][1] as URLSearchParams
    expect(params.get('password')).toBe('secret')
  })

  it('createPublicShare includes expireDate when provided', async () => {
    mockOcsPost.mockResolvedValue({
      data: { ocs: { data: { url: 'https://opencloud.example.com/s/ABC' } } },
    })

    const { createPublicShare } = useOpenCloudAPI()
    await createPublicShare('/file.pdf', { expireDate: '2026-04-10' })

    const params = mockOcsPost.mock.calls[0][1] as URLSearchParams
    expect(params.get('expireDate')).toBe('2026-04-10')
  })

  it('createPublicShare omits password and expireDate when not provided', async () => {
    mockOcsPost.mockResolvedValue({
      data: { ocs: { data: { url: 'https://opencloud.example.com/s/ABC' } } },
    })

    const { createPublicShare } = useOpenCloudAPI()
    await createPublicShare('/file.pdf')

    const params = mockOcsPost.mock.calls[0][1] as URLSearchParams
    expect(params.has('password')).toBe(false)
    expect(params.has('expireDate')).toBe(false)
  })
})