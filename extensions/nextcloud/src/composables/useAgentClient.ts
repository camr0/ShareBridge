import axios from '@nextcloud/axios'
import { generateUrl } from '@nextcloud/router'
import type { CreateShareParams, CreateShareResult, Share, AgentSettings } from '../types'

/**
 * Calls the NC PHP proxy endpoints instead of the agent directly.
 *
 * Two reasons for the proxy:
 *  1. NC's CSP (connect-src 'self') blocks direct browser→agent requests.
 *  2. Mixed content: NC is HTTPS but the agent may be HTTP on the local network.
 *
 * The proxy uses PHP curl (not IClientService) to reach local IPs without
 * requiring allow_local_remote_servers. The API key stays server-side.
 */
export const useAgentClient = () => {
    const listShares = async (fileId?: string): Promise<Share[]> => {
        const url = generateUrl('/apps/sharebridge/api/agent/shares')
        const params = fileId ? { file_id: fileId } : {}
        const response = await axios.get(url, { params })
        return response.data
    }

    const createShare = async (params: CreateShareParams): Promise<CreateShareResult> => {
        const url = generateUrl('/apps/sharebridge/api/agent/shares')
        const response = await axios.post(url, {
            ...params,
            share_type: 'nextcloud',
        })
        return response.data
    }

    const revokeShare = async (code: string): Promise<void> => {
        const url = generateUrl(`/apps/sharebridge/api/agent/shares/${encodeURIComponent(code)}`)
        await axios.delete(url)
    }

    const getSettings = async (): Promise<AgentSettings> => {
        const url = generateUrl('/apps/sharebridge/api/agent/settings')
        const response = await axios.get(url)
        return response.data
    }

    return { listShares, createShare, revokeShare, getSettings }
}
