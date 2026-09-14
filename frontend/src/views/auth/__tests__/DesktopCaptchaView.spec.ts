import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent, h } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import DesktopCaptchaView from '@/views/auth/DesktopCaptchaView.vue'

const { getIsolatedPublicSettings, verifyAction } = vi.hoisted(() => ({
  getIsolatedPublicSettings: vi.fn(),
  verifyAction: vi.fn()
}))

vi.mock('@/api/publicSettings', () => ({ getIsolatedPublicSettings }))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

const CaptchaChallengeStub = defineComponent({
  name: 'CaptchaChallenge',
  emits: ['verify', 'expire', 'error'],
  setup(_props, { expose }) {
    expose({ verifyAction })
    return () => h('div', { 'data-testid': 'captcha-challenge' })
  }
})

function mountView() {
  return mount(DesktopCaptchaView, {
    global: { stubs: { CaptchaChallenge: CaptchaChallengeStub } }
  })
}

function installBridge(overrides: Record<string, unknown> = {}) {
  const bridge = {
    getChallenge: vi.fn().mockResolvedValue({ nonce: 'nonce-private-value' }),
    submit: vi.fn().mockResolvedValue(undefined),
    ...overrides
  }
  window.ainoCaptcha = bridge
  return bridge
}

const disabledSettings = {
  turnstile_enabled: false,
  turnstile_site_key: '',
  tencent_captcha_enabled: false,
  tencent_captcha_app_id: '',
  aliyun_captcha_enabled: false,
  aliyun_captcha_scene_id: '',
  aliyun_captcha_prefix: ''
}

describe('DesktopCaptchaView', () => {
  beforeEach(() => {
    delete window.ainoCaptcha
    getIsolatedPublicSettings.mockReset()
    verifyAction.mockReset()
  })

  it('shows unavailable without the narrow bridge and performs no request', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-testid="desktop-captcha-unavailable"]').exists()).toBe(true)
    expect(getIsolatedPublicSettings).not.toHaveBeenCalled()
  })

  it('rejects an invalid challenge nonce without exposing or submitting it', async () => {
    const bridge = installBridge({ getChallenge: vi.fn().mockResolvedValue({ nonce: '  ' }) })
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-testid="desktop-captcha-unavailable"]').exists()).toBe(true)
    expect(bridge.submit).not.toHaveBeenCalled()
    expect(getIsolatedPublicSettings).not.toHaveBeenCalled()
  })

  it('submits a Turnstile proof exactly once without rendering the nonce', async () => {
    const bridge = installBridge()
    getIsolatedPublicSettings.mockResolvedValue({
      ...disabledSettings,
      turnstile_enabled: true,
      turnstile_site_key: 'site-key'
    })
    const wrapper = mountView()
    await flushPromises()

    const captcha = wrapper.getComponent(CaptchaChallengeStub)
    captcha.vm.$emit('verify', 'turnstile-proof', '')
    captcha.vm.$emit('verify', 'duplicate-proof', '')
    await flushPromises()

    expect(bridge.submit).toHaveBeenCalledTimes(1)
    expect(bridge.submit).toHaveBeenCalledWith({
      nonce: 'nonce-private-value',
      proof: { turnstile_token: 'turnstile-proof' }
    })
    expect(wrapper.text()).not.toContain('nonce-private-value')
  })

  it.each([
    ['Tencent', { tencent_captcha_enabled: true, tencent_captcha_app_id: 'app-id' }, { token: 'ticket', randstr: '@rand' }, { tencent_captcha_ticket: 'ticket', tencent_captcha_randstr: '@rand' }],
    ['Aliyun', { aliyun_captcha_enabled: true, aliyun_captcha_scene_id: 'scene', aliyun_captcha_prefix: 'prefix' }, { token: 'verify-param', randstr: '' }, { turnstile_token: 'verify-param' }]
  ])('maps a supported %s action challenge proof', async (_name, settings, result, expectedProof) => {
    const bridge = installBridge()
    getIsolatedPublicSettings.mockResolvedValue({ ...disabledSettings, ...settings })
    verifyAction.mockResolvedValue(result)
    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-testid="desktop-captcha-start"]').trigger('click')
    await flushPromises()

    expect(bridge.submit).toHaveBeenCalledWith({
      nonce: 'nonce-private-value',
      proof: expectedProof
    })
  })

  it('submits an empty proof when captcha is disabled and refuses misconfiguration', async () => {
    const disabledBridge = installBridge()
    getIsolatedPublicSettings.mockResolvedValueOnce(disabledSettings)
    const disabledWrapper = mountView()
    await flushPromises()

    expect(disabledWrapper.find('[data-testid="captcha-challenge"]').exists()).toBe(false)
    expect(disabledBridge.submit).toHaveBeenCalledWith({ nonce: 'nonce-private-value', proof: {} })

    const invalidBridge = installBridge()
    getIsolatedPublicSettings.mockResolvedValueOnce({
      ...disabledSettings,
      turnstile_enabled: true,
      turnstile_site_key: ''
    })
    const invalidWrapper = mountView()
    await flushPromises()

    expect(invalidWrapper.get('[data-testid="desktop-captcha-unavailable"]').exists()).toBe(true)
    expect(invalidBridge.submit).not.toHaveBeenCalled()
  })
})
