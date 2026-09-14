import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import PhoneBindingForm from '@/components/user/profile/PhoneBindingForm.vue'

const { sendPhoneBindingCode, bindPhoneIdentity, showError } = vi.hoisted(() => ({
  sendPhoneBindingCode: vi.fn(),
  bindPhoneIdentity: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/user', () => ({ sendPhoneBindingCode, bindPhoneIdentity }))
vi.mock('@/stores', () => ({
  useAppStore: () => ({ showError, showSuccess: vi.fn() })
}))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key })
}))

const user = {
  id: 7,
  username: 'alice',
  email: 'alice@example.com',
  role: 'user',
  balance: 42,
  concurrency: 2,
  status: 'active',
  allowed_groups: null,
  balance_notify_enabled: true,
  balance_notify_threshold: null,
  balance_notify_extra_emails: [],
  created_at: '2026-09-15T00:00:00Z',
  updated_at: '2026-09-15T00:00:00Z'
} as const

function mountForm(overrides: Record<string, unknown> = {}) {
  return mount(PhoneBindingForm, {
    props: { codeLength: 6, ...overrides },
    global: {
      stubs: {
        TotpStepUpDialog: {
          props: ['controller'],
          template: '<button v-if="controller.visible.value" data-testid="complete-step-up" @click="controller.onVerified()">verify</button>'
        }
      }
    }
  })
}

describe('PhoneBindingForm', () => {
  beforeEach(() => {
    vi.useRealTimers()
    sendPhoneBindingCode.mockReset()
    bindPhoneIdentity.mockReset()
    showError.mockReset()
  })

  it('retries through TOTP step-up and updates only from the authoritative user response', async () => {
    sendPhoneBindingCode
      .mockRejectedValueOnce({ reason: 'STEP_UP_REQUIRED' })
      .mockResolvedValueOnce({ challenge_id: 'challenge', expires_in: 300, retry_after: 60, delivery: 'submitted' })
    bindPhoneIdentity.mockResolvedValue({
      ...user,
      auth_bindings: { phone: { bound: true, subject_hint: '+86 139****0000' } }
    })
    const wrapper = mountForm()
    await wrapper.get('[data-testid="phone-binding-phone"]').setValue('+86 139 0000 0000')
    await wrapper.get('[data-testid="phone-binding-send"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="complete-step-up"]').exists()).toBe(true)

    await wrapper.get('[data-testid="complete-step-up"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="phone-binding-code"]').setValue('123456')
    await wrapper.get('[data-testid="phone-binding-submit"]').trigger('click')
    await flushPromises()

    expect(sendPhoneBindingCode).toHaveBeenLastCalledWith({ phone: '+8613900000000' })
    expect(bindPhoneIdentity).toHaveBeenCalledWith({
      phone: '+8613900000000', challenge_id: 'challenge', code: '123456'
    })
    expect(bindPhoneIdentity.mock.calls[0][0]).not.toHaveProperty('user_id')
    expect(wrapper.emitted('updated')?.[0]?.[0]).toMatchObject({ id: 7, balance: 42 })
  })

  it('passes the existing captcha proof contract when requesting a binding code', async () => {
    sendPhoneBindingCode.mockResolvedValue({ challenge_id: 'challenge', expires_in: 300, retry_after: 60, delivery: 'submitted' })
    const wrapper = mountForm({
      getCaptchaProof: vi.fn().mockResolvedValue({
        tencent_captcha_ticket: 'ticket',
        tencent_captcha_randstr: 'rand'
      })
    })
    await wrapper.get('[data-testid="phone-binding-phone"]').setValue('13900000000')
    await wrapper.get('[data-testid="phone-binding-send"]').trigger('click')
    await flushPromises()

    expect(sendPhoneBindingCode).toHaveBeenCalledWith({
      phone: '+8613900000000',
      tencent_captcha_ticket: 'ticket',
      tencent_captcha_randstr: 'rand'
    })
  })

  it('admits only one send while captcha proof acquisition is pending', async () => {
    let resolveProof!: (value: { turnstile_token: string }) => void
    const getCaptchaProof = vi.fn(() => new Promise<{ turnstile_token: string }>((resolve) => { resolveProof = resolve }))
    sendPhoneBindingCode.mockResolvedValue({ challenge_id: 'challenge', expires_in: 300, retry_after: 60, delivery: 'submitted' })
    const wrapper = mountForm({ getCaptchaProof })
    await wrapper.get('[data-testid="phone-binding-phone"]').setValue('13900000000')

    await wrapper.get('[data-testid="phone-binding-send"]').trigger('click')
    await wrapper.get('[data-testid="phone-binding-send"]').trigger('click')
    resolveProof({ turnstile_token: 'captcha-proof' })
    await flushPromises()

    expect(getCaptchaProof).toHaveBeenCalledTimes(1)
    expect(sendPhoneBindingCode).toHaveBeenCalledTimes(1)
  })

  it('shows a re-login path when recent authentication is required', async () => {
    sendPhoneBindingCode.mockRejectedValue({ reason: 'RECENT_AUTH_REQUIRED' })
    const wrapper = mountForm()
    await wrapper.get('[data-testid="phone-binding-phone"]').setValue('13900000000')
    await wrapper.get('[data-testid="phone-binding-send"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="phone-binding-relogin"]').attributes('href')).toBe('/login?redirect=/profile')
  })

  it('surfaces a binding conflict without changing the current profile', async () => {
    sendPhoneBindingCode.mockResolvedValue({ challenge_id: 'challenge', expires_in: 300, retry_after: 60, delivery: 'submitted' })
    bindPhoneIdentity.mockRejectedValue({ reason: 'PHONE_ALREADY_BOUND', message: 'Phone is already bound' })
    const wrapper = mountForm()
    await wrapper.get('[data-testid="phone-binding-phone"]').setValue('13900000000')
    await wrapper.get('[data-testid="phone-binding-send"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="phone-binding-code"]').setValue('123456')
    await wrapper.get('[data-testid="phone-binding-submit"]').trigger('click')
    await flushPromises()

    expect(wrapper.emitted('updated')).toBeUndefined()
    expect(showError).toHaveBeenCalledWith('Phone is already bound')
  })

  it('does not adopt a delayed challenge after the requested phone changes', async () => {
    let resolveSend!: (value: unknown) => void
    sendPhoneBindingCode.mockImplementation(() => new Promise((resolve) => { resolveSend = resolve }))
    const wrapper = mountForm()
    await wrapper.get('[data-testid="phone-binding-phone"]').setValue('13900000000')
    await wrapper.get('[data-testid="phone-binding-send"]').trigger('click')
    await wrapper.get('[data-testid="phone-binding-phone"]').setValue('13800000000')
    resolveSend({ challenge_id: 'stale', expires_in: 300, retry_after: 60, delivery: 'submitted' })
    await flushPromises()

    expect(wrapper.get('[data-testid="phone-binding-submit"]').attributes('disabled')).toBeDefined()
    expect(wrapper.find('[data-testid="phone-binding-delivery"]').exists()).toBe(false)
  })
})
