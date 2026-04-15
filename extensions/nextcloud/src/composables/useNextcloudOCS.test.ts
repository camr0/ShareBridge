import { describe, it, expect, vi, beforeEach } from 'vitest'
import axios from '@nextcloud/axios'
import { useNextcloudOCS } from './useNextcloudOCS'

vi.mock('@nextcloud/axios', () => ({
    default: {
        post: vi.fn(),
        delete: vi.fn(),
        put: vi.fn(),
        get: vi.fn(),
    },
}))

vi.mock('@nextcloud/router', () => ({
    generateUrl: (path: string) => path,
}))

describe('useNextcloudOCS', () => {
    beforeEach(() => {
        vi.mocked(axios.post).mockReset()
        vi.mocked(axios.delete).mockReset()
        vi.mocked(axios.put).mockReset()
        vi.mocked(axios.get).mockReset()
    })

    describe('createOCSShare', () => {
        it('POSTs to OCS shares endpoint with shareType=3 and path', async () => {
            vi.mocked(axios.post).mockResolvedValue({
                data: { ocs: { meta: { statuscode: 200 }, data: { id: 42, url: 'https://nc.example.com/s/abc' } } },
            } as never)

            const { createOCSShare } = useNextcloudOCS()
            await createOCSShare('/Documents/report.pdf', 24)

            expect(vi.mocked(axios.post)).toHaveBeenCalledWith(
                '/ocs/v2.php/apps/files_sharing/api/v1/shares',
                expect.stringContaining('shareType=3'),
                expect.objectContaining({ headers: { 'Content-Type': 'application/x-www-form-urlencoded' } })
            )
        })

        it('includes the file path in POST body', async () => {
            vi.mocked(axios.post).mockResolvedValue({
                data: { ocs: { meta: { statuscode: 200 }, data: { id: 42, url: 'https://nc.example.com/s/abc' } } },
            } as never)

            const { createOCSShare } = useNextcloudOCS()
            await createOCSShare('/Documents/report.pdf', 24)

            const body = vi.mocked(axios.post).mock.calls[0][1] as string
            expect(body).toContain('path=%2FDocuments%2Freport.pdf')
        })

        it('returns shareUrl and shareId from OCS response', async () => {
            vi.mocked(axios.post).mockResolvedValue({
                data: { ocs: { meta: { statuscode: 200 }, data: { id: 42, url: 'https://nc.example.com/s/abc' } } },
            } as never)

            const { createOCSShare } = useNextcloudOCS()
            const result = await createOCSShare('/Documents/report.pdf', 24)

            expect(result.shareUrl).toBe('https://nc.example.com/s/abc')
            expect(result.shareId).toBe('42')
        })

        it('sets expireDate to a YYYY-MM-DD string in the future', async () => {
            vi.mocked(axios.post).mockResolvedValue({
                data: { ocs: { meta: { statuscode: 200 }, data: { id: 1, url: 'https://nc.example.com/s/x' } } },
            } as never)

            const { createOCSShare } = useNextcloudOCS()
            await createOCSShare('/file.pdf', 1) // 1 hour expiry

            const body = vi.mocked(axios.post).mock.calls[0][1] as string
            const match = body.match(/expireDate=(\d{4}-\d{2}-\d{2})/)
            expect(match).not.toBeNull()
            // The date must be today or later
            const expireDate = new Date(match![1])
            expect(expireDate.getTime()).toBeGreaterThanOrEqual(Date.now() - 86400000)
        })

        it('throws PASSWORD_REQUIRED when OCS meta indicates password enforcement', async () => {
            vi.mocked(axios.post).mockResolvedValue({
                data: { ocs: { meta: { statuscode: 403, message: 'Password protection is enforced' }, data: null } },
            } as never)

            const { createOCSShare } = useNextcloudOCS()
            await expect(createOCSShare('/file.pdf', 24)).rejects.toThrow('PASSWORD_REQUIRED')
        })

        it('throws OCS_SHARE_FAILED when OCS response has no url', async () => {
            vi.mocked(axios.post).mockResolvedValue({
                data: { ocs: { meta: { statuscode: 500, message: 'Internal error' }, data: null } },
            } as never)

            const { createOCSShare } = useNextcloudOCS()
            await expect(createOCSShare('/file.pdf', 24)).rejects.toThrow('OCS_SHARE_FAILED')
        })
    })

    describe('deleteOCSShare', () => {
        it('DELETEs the OCS share by ID', async () => {
            vi.mocked(axios.delete).mockResolvedValue({} as never)

            const { deleteOCSShare } = useNextcloudOCS()
            await deleteOCSShare('42')

            expect(vi.mocked(axios.delete)).toHaveBeenCalledWith(
                '/ocs/v2.php/apps/files_sharing/api/v1/shares/42',
                expect.anything()
            )
        })
    })

    describe('saveNcShareId', () => {
        it('PUTs code→shareId mapping to the PHP endpoint', async () => {
            vi.mocked(axios.put).mockResolvedValue({ data: { status: 'ok' } } as never)

            const { saveNcShareId } = useNextcloudOCS()
            await saveNcShareId('ABC123', '42')

            expect(vi.mocked(axios.put)).toHaveBeenCalledWith(
                '/apps/sharebridge/api/shares/ABC123/nc-share-id',
                { nc_share_id: '42' }
            )
        })
    })

    describe('getNcShareId', () => {
        it('GETs the stored shareId for a code', async () => {
            vi.mocked(axios.get).mockResolvedValue({
                data: { nc_share_id: '42' },
            } as never)

            const { getNcShareId } = useNextcloudOCS()
            const result = await getNcShareId('ABC123')

            expect(vi.mocked(axios.get)).toHaveBeenCalledWith(
                '/apps/sharebridge/api/shares/ABC123/nc-share-id'
            )
            expect(result).toBe('42')
        })
    })

    describe('deleteNcShareId', () => {
        it('DELETEs the mapping entry for a code', async () => {
            vi.mocked(axios.delete).mockResolvedValue({} as never)

            const { deleteNcShareId } = useNextcloudOCS()
            await deleteNcShareId('ABC123')

            expect(vi.mocked(axios.delete)).toHaveBeenCalledWith(
                '/apps/sharebridge/api/shares/ABC123/nc-share-id'
            )
        })
    })
})