import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createI18n } from 'vue-i18n'
import { useAppStore, useAuthStore } from '@/stores'
import en from '@/i18n/locales/en/misc'
import zh from '@/i18n/locales/zh/misc'
import VersionBadge from '../VersionBadge.vue'

enableAutoUnmount(afterEach)

describe('VersionBadge deployment modes', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    useAuthStore().$patch({
      user: {
        id: 1,
        username: 'admin',
        email: 'admin@example.com',
        role: 'admin',
        balance: 100,
        concurrency: 5,
        status: 'active',
        allowed_groups: null,
        created_at: '2026-09-17',
        updated_at: '2026-09-17'
      }
    })
    useAppStore().$patch({
      versionLoaded: true,
      currentVersion: '0.2.5-aino-private-20260917',
      latestVersion: '0.2.6',
      hasUpdate: false,
      buildType: 'manual',
      releaseInfo: {
        name: 'v0.2.6',
        body: 'Upstream release',
        html_url: 'https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.6',
        published_at: '2026-09-17T00:00:00Z'
      }
    })
  })

  function mountBadge(locale = 'en') {
    const messages = Object.fromEntries(
      Object.entries({ en, zh }).map(([code, localeMessages]) => [code, {
        version: Object.fromEntries(
          Object.entries(localeMessages.version).map(([key, value]) => [key, () => value])
        )
      }])
    )
    return mount(VersionBadge, {
      global: {
        plugins: [createI18n({ legacy: false, locale, messages })]
      }
    })
  }

  it.each([
    ['en', 'Manually deployed build'],
    ['zh', '手动部署版本']
  ])('identifies a manual build without claiming it is current in %s', async (locale, status) => {
    const wrapper = mountBadge(locale)
    expect(wrapper.get('button').attributes('title')).toBe(status)
    await wrapper.get('button').trigger('click')

    expect(wrapper.text()).toContain(status)
    expect(wrapper.text()).toContain('v0.2.5-aino-private-20260917')
    expect(wrapper.text()).not.toContain(en.version.upToDate)
    expect(wrapper.text()).not.toContain(zh.version.upToDate)
    expect(wrapper.findAll('button').map(button => button.attributes('title'))).toEqual([
      status,
      locale === 'zh' ? '刷新' : 'Refresh'
    ])
    expect(wrapper.find('a').exists()).toBe(false)
    expect(wrapper.find('code').exists()).toBe(false)
  })

  it('suppresses cached upstream updates and actions for manual builds', async () => {
    useAppStore().hasUpdate = true
    const wrapper = mountBadge()
    await wrapper.get('button').trigger('click')

    expect(wrapper.text()).not.toContain(en.version.updateAvailable)
    expect(wrapper.text()).not.toContain('0.2.6')
    expect(wrapper.text()).not.toContain(en.version.sourceModeHint)
    expect(wrapper.text()).not.toContain(en.version.rollback)
    expect(wrapper.find('a').exists()).toBe(false)
    expect(wrapper.find('code').exists()).toBe(false)
    expect(wrapper.findAll('button')).toHaveLength(2)
  })

  it('keeps the update action and changelog available for release builds', async () => {
    useAppStore().$patch({ buildType: 'release', hasUpdate: true })
    const wrapper = mountBadge()
    await wrapper.get('button').trigger('click')

    expect(wrapper.text()).toContain(en.version.updateAvailable)
    expect(wrapper.findAll('button').some(button => button.text() === 'Update Now')).toBe(true)
    expect(wrapper.get('a').text()).toBe('View Changelog')
  })

  it('keeps the git update hint for source builds', async () => {
    useAppStore().$patch({ buildType: 'source', hasUpdate: true })
    const wrapper = mountBadge()
    await wrapper.get('button').trigger('click')

    expect(wrapper.text()).toContain('Source build, use git pull to update')
    expect(wrapper.get('a').attributes('href')).toBe(
      'https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.6'
    )
  })

  it('keeps the rollback entry for current source and release builds', async () => {
    const store = useAppStore()
    store.buildType = 'release'
    const wrapper = mountBadge()
    await wrapper.get('button').trigger('click')

    expect(wrapper.text()).toContain("You're running the latest version.")
    expect(wrapper.text()).toContain('Version Rollback')
    store.buildType = 'source'
    await wrapper.vm.$nextTick()
    const rollbackButton = wrapper.findAll('button').find(button => button.text() === 'Version Rollback')!
    await rollbackButton.trigger('click')
    expect(wrapper.text()).toContain('Online rollback is not available for source builds')

    store.buildType = 'manual'
    await wrapper.vm.$nextTick()
    expect(wrapper.text()).not.toContain('Version Rollback')
    expect(wrapper.text()).not.toContain('Online rollback is not available for source builds')
    expect(wrapper.find('a').exists()).toBe(false)
  })
})
