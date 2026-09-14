import type { ApiResponse, PublicSettings } from '@/types'
import { buildApiUrl } from './url'

export async function getIsolatedPublicSettings(): Promise<PublicSettings> {
  const response = await fetch(buildApiUrl('/settings/public'), {
    method: 'GET',
    credentials: 'omit',
    cache: 'no-store',
    headers: { Accept: 'application/json' }
  })
  if (!response.ok) throw new Error('Public settings are unavailable')

  const payload = await response.json() as ApiResponse<PublicSettings> | PublicSettings
  if ('code' in payload) {
    if (payload.code !== 0 || !payload.data) throw new Error('Public settings are unavailable')
    return payload.data
  }
  return payload
}
