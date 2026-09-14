import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import App from '@/App.vue'

const { useAuthStore, fetchPublicSettings, getSetupStatus } = vi.hoisted(() => ({
  useAuthStore: vi.fn(() => ({ isAuthenticated: false, isAdmin: false })),
  fetchPublicSettings: vi.fn(),
  getSetupStatus: vi.fn()
}))

vi.mock('vue-router', async () => {
  const { defineComponent, h } = await import('vue')
  return {
    RouterView: defineComponent(() => () => h('div', { 'data-testid': 'isolated-router-view' })),
    useRoute: () => ({ path: '/desktop/captcha', fullPath: '/desktop/captcha', meta: { isolatedPublic: true } }),
    useRouter: () => ({ afterEach: vi.fn(), replace: vi.fn() })
  }
})
vi.mock('@/stores', () => ({
  useAuthStore,
  useAppStore: () => ({ cachedPublicSettings: null, siteLogo: '', siteName: 'Aino', fetchPublicSettings }),
  useSubscriptionStore: vi.fn(),
  useAnnouncementStore: vi.fn(),
  useAdminComplianceStore: vi.fn(),
  useAdminSettingsStore: vi.fn()
}))
vi.mock('@/api/setup', () => ({ getSetupStatus }))

describe('App desktop captcha isolation', () => {
  it('mounts only the router view without normal account effects', () => {
    localStorage.setItem('auth_token', 'stale-token')

    const wrapper = mount(App)

    expect(wrapper.get('[data-testid="isolated-router-view"]').exists()).toBe(true)
    expect(useAuthStore).not.toHaveBeenCalled()
    expect(getSetupStatus).not.toHaveBeenCalled()
    expect(fetchPublicSettings).not.toHaveBeenCalled()
  })
})
