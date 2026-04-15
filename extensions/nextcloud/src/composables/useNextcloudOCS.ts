import axios from '@nextcloud/axios'
import { generateUrl } from '@nextcloud/router'

export interface OCSShareResult {
    shareUrl: string
    shareId: string
}

// Compute expireDate for OCS: use the UTC calendar date of the expiry.
// OCS accepts YYYY-MM-DD only — sub-day expiry is not supported.
// The NC public link may outlive the ShareBridge share by up to ~24h, which is
// acceptable because ShareBridge is the access gatekeeper.
const ocsExpireDate = (expiryHours: number): string => {
    const expiry = new Date(Date.now() + expiryHours * 3600 * 1000)
    return expiry.toISOString().slice(0, 10) // YYYY-MM-DD
}

export const useNextcloudOCS = () => {
    const createOCSShare = async (filePath: string, expiryHours: number): Promise<OCSShareResult> => {
        const params = new URLSearchParams({
            shareType:  '3',
            path:       filePath,
            expireDate: ocsExpireDate(expiryHours),
        })

        const response = await axios.post(
            '/ocs/v2.php/apps/files_sharing/api/v1/shares',
            params.toString(),
            {
                headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
                params:  { format: 'json' },
            }
        )

        const ocsData = response.data?.ocs?.data
        if (!ocsData?.url) {
            const message: string = response.data?.ocs?.meta?.message ?? ''
            if (message.toLowerCase().includes('password')) {
                throw new Error('PASSWORD_REQUIRED')
            }
            throw new Error('OCS_SHARE_FAILED')
        }

        return {
            shareUrl: ocsData.url,
            shareId:  String(ocsData.id),
        }
    }

    const deleteOCSShare = async (shareId: string): Promise<void> => {
        await axios.delete(
            `/ocs/v2.php/apps/files_sharing/api/v1/shares/${shareId}`,
            { params: { format: 'json' } }
        )
    }

    const saveNcShareId = async (code: string, ncShareId: string): Promise<void> => {
        await axios.put(
            generateUrl(`/apps/sharebridge/api/shares/${code}/nc-share-id`),
            { nc_share_id: ncShareId }
        )
    }

    const getNcShareId = async (code: string): Promise<string> => {
        const response = await axios.get(
            generateUrl(`/apps/sharebridge/api/shares/${code}/nc-share-id`)
        )
        return response.data.nc_share_id
    }

    const deleteNcShareId = async (code: string): Promise<void> => {
        await axios.delete(generateUrl(`/apps/sharebridge/api/shares/${code}/nc-share-id`))
    }

    return { createOCSShare, deleteOCSShare, saveNcShareId, getNcShareId, deleteNcShareId }
}