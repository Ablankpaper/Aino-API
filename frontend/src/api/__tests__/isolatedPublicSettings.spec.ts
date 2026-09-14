import { beforeEach, describe, expect, it, vi } from 'vitest'
import { getIsolatedPublicSettings } from '@/api/publicSettings'

describe('isolated public settings request', () => {
  beforeEach(() => {
    localStorage.setItem('auth_token', 'must-not-leak')
    vi.restoreAllMocks()
  })

  it('omits credentials and authentication headers', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(JSON.stringify({
      code: 0,
      data: { turnstile_enabled: false, turnstile_site_key: '' }
    }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' }
    }))

    await getIsolatedPublicSettings()

    expect(fetchMock).toHaveBeenCalledWith('/api/v1/settings/public', expect.objectContaining({
      credentials: 'omit'
    }))
    const headers = new Headers(fetchMock.mock.calls[0]?.[1]?.headers)
    expect(headers.has('Authorization')).toBe(false)
  })
})
