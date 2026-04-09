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

    const response = await clientService.httpAuthenticated.post(
      '/apps/files_sharing/api/v1/shares',
      params
    )
    return response.data.ocs.data.url as string
  }

  return { createPublicShare }
}