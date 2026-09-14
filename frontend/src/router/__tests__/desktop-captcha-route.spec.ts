import { beforeAll, describe, expect, it, vi } from 'vitest'

type NavigationGuard = (
  to: Record<string, any>,
  from: Record<string, any>,
  next: ReturnType<typeof vi.fn>
) => Promise<void>

const harness = vi.hoisted(() => ({
  guard: null as NavigationGuard | null,
  routes: [] as Array<Record<string, any>>
}))
const useAuthStore = vi.hoisted(() => vi.fn())

vi.mock('vue-router', () => ({
  createWebHistory: vi.fn(() => ({})),
  createRouter: vi.fn((options: { routes: Array<Record<string, any>> }) => {
    harness.routes = options.routes
    return {
      beforeEach: vi.fn((guard: NavigationGuard) => { harness.guard = guard }),
      afterEach: vi.fn(),
      onError: vi.fn()
    }
  })
}))
vi.mock('@/stores/auth', () => ({ useAuthStore }))
vi.mock('@/stores/app', () => ({ useAppStore: vi.fn() }))
vi.mock('@/stores/adminSettings', () => ({ useAdminSettingsStore: vi.fn() }))
vi.mock('@/stores/adminCompliance', () => ({ useAdminComplianceStore: vi.fn() }))
vi.mock('@/composables/useNavigationLoading', () => ({
  useNavigationLoadingState: () => ({ startNavigation: vi.fn(), endNavigation: vi.fn() })
}))
vi.mock('@/composables/useRoutePrefetch', () => ({ useRoutePrefetch: vi.fn() }))

describe('desktop captcha route isolation', () => {
  beforeAll(async () => {
    await import('@/router')
  })

  it('registers an isolated public route and bypasses auth restoration', async () => {
    const route = harness.routes.find((candidate) => candidate.path === '/desktop/captcha')
    expect(route?.meta).toMatchObject({ requiresAuth: false, isolatedPublic: true })

    const next = vi.fn()
    await harness.guard?.({ path: route?.path, fullPath: route?.path, meta: route?.meta }, {}, next)

    expect(next).toHaveBeenCalledWith()
    expect(useAuthStore).not.toHaveBeenCalled()
  })
})
