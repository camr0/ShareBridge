import { useClientService } from '@opencloud-eu/web-pkg'

interface CreatePublicShareOptions {
  password?: string
  expireDate?: string
}

export const useOpenCloudAPI = () => {
  const clientService = useClientService()

  const createPublicShare = async (
    path: string,
    options: CreatePublicShareOptions = {}
  ): Promise<string> => {
    const params = new URLSearchParams({ shareType: '3', path })
    if (options.password) params.set('password', options.password)
    if (options.expireDate) params.set('expireDate', options.expireDate)

    // Note: verify exact method name against your @opencloud-eu/web-pkg version.
    // It may be clientService.httpAuthenticated.post() or clientService.ocs.post().
    // Check: node_modules/@opencloud-eu/web-pkg/dist/index.d.ts for ClientService type.
    const response = await clientService.ocs.post(
      '/apps/files_sharing/api/v1/shares',
      params
    )
    return response.ocs.data.url as string
  }

  return { createPublicShare }
}