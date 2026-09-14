import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import PhoneLoginForm from '@/components/auth/PhoneLoginForm.vue'

const { sendPhoneCode, loginWithPhone, showError } = vi.hoisted(() => ({
  sendPhoneCode: vi.fn(),
  loginWithPhone: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/auth', () => ({
  sendPhoneCode,
  isTotp2FARequired: (response: { requires_2fa?: boolean }) => response.requires_2fa === true
}))

vi.mock('@/stores', () => ({
  useAuthStore: () => ({ loginWithPhone }),
  useAppStore: () => ({ showError, showSuccess: vi.fn() })
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) =>
      key === 'auth.resendCountdown'
          ? `${key} ${String(params?.countdown || '')}`
          : key
  })
}))

function mountForm(overrides: Record<string, unknown> = {}) {
  return mount(PhoneLoginForm, {
    props: {
      codeLength: 6,
      registrationEnabled: true,
      invitationCodeEnabled: false,
      promoCodeEnabled: false,
      agreementRevision: 'agreement-v3',
      agreementAccepted: true,
      getCaptchaProof: vi.fn().mockResolvedValue({ turnstile_token: 'captcha-proof' }),
      ...overrides
    },
    global: { stubs: { Icon: true } }
  })
}

function authResponse() {
  return {
    access_token: 'token',
    token_type: 'Bearer',
    user: { id: 7 }
  }
}

describe('PhoneLoginForm', () => {
  beforeEach(() => {
    vi.useRealTimers()
    sendPhoneCode.mockReset()
    loginWithPhone.mockReset()
    showError.mockReset()
  })

  it('canonicalizes accepted +86 formatting and ignores a reply after the phone changes', async () => {
    let resolveFirst!: (value: unknown) => void
    sendPhoneCode.mockImplementationOnce(() => new Promise((resolve) => { resolveFirst = resolve }))
    const wrapper = mountForm()

    await wrapper.get('[data-testid="phone-login-phone"]').setValue('+86 139 0000 0000')
    await wrapper.get('[data-testid="phone-login-send"]').trigger('click')
    expect(sendPhoneCode).toHaveBeenCalledWith({
      phone: '+8613900000000',
      turnstile_token: 'captcha-proof'
    })

    await wrapper.get('[data-testid="phone-login-phone"]').setValue('13800000000')
    resolveFirst({ challenge_id: 'stale', expires_in: 300, retry_after: 60, delivery: 'submitted' })
    await flushPromises()

    expect(wrapper.find('[data-testid="phone-login-delivery"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="phone-login-submit"]').attributes('disabled')).toBeDefined()
  })

  it('normalizes a pasted country prefix for display and submits the code with Enter', async () => {
    sendPhoneCode.mockResolvedValue({ challenge_id: 'challenge', expires_in: 300, retry_after: 60, delivery: 'submitted' })
    loginWithPhone.mockResolvedValue(authResponse())
    const wrapper = mountForm()
    const phoneInput = wrapper.get('[data-testid="phone-login-phone"]')

    await phoneInput.setValue('+86 139 0000 0000')
    await phoneInput.trigger('blur')
    expect((phoneInput.element as HTMLInputElement).value).toBe('13900000000')

    await wrapper.get('[data-testid="phone-login-send"]').trigger('click')
    await flushPromises()
    const codeInput = wrapper.get('[data-testid="phone-login-code"]')
    await codeInput.setValue('123456')
    await codeInput.trigger('keydown.enter')
    await flushPromises()

    expect(loginWithPhone).toHaveBeenCalledTimes(1)
    expect(loginWithPhone).toHaveBeenCalledWith(expect.objectContaining({
      phone: '+8613900000000',
      code: '123456'
    }))
  })

  it('offers existing account users an email sign-in path', async () => {
    const wrapper = mountForm()

    await wrapper.get('[data-testid="phone-login-existing-account"]').trigger('click')

    expect(wrapper.emitted('use-existing-account')).toHaveLength(1)
  })

  it('expires the challenge using the server lifetime and allows a network retry', async () => {
    vi.useFakeTimers()
    sendPhoneCode
      .mockRejectedValueOnce({ status: 0, message: 'Network error' })
      .mockResolvedValueOnce({ challenge_id: 'fresh', expires_in: 2, retry_after: 1, delivery: 'submitted' })
    const wrapper = mountForm()
    await wrapper.get('[data-testid="phone-login-phone"]').setValue('13900000000')

    await wrapper.get('[data-testid="phone-login-send"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="phone-login-send"]').attributes('disabled')).toBeUndefined()

    await wrapper.get('[data-testid="phone-login-send"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="phone-login-delivery"]').text()).toBe('auth.phoneDeliverySubmitted')
    expect(wrapper.get('[data-testid="phone-login-delivery"]').text()).not.toContain('submitted')

    await vi.advanceTimersByTimeAsync(2100)
    expect(wrapper.get('[data-testid="phone-login-expired"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="phone-login-submit"]').attributes('disabled')).toBeDefined()
  })

  it('uses Retry-After metadata after a 429 and recovers when it elapses', async () => {
    vi.useFakeTimers()
    sendPhoneCode.mockRejectedValue({
      status: 429,
      reason: 'SMS_RATE_LIMITED',
      metadata: { retry_after: '3' }
    })
    const wrapper = mountForm()
    await wrapper.get('[data-testid="phone-login-phone"]').setValue('13900000000')

    await wrapper.get('[data-testid="phone-login-send"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="phone-login-send"]').text()).toContain('3')
    expect(wrapper.get('[data-testid="phone-login-send"]').attributes('disabled')).toBeDefined()

    await vi.advanceTimersByTimeAsync(3100)
    expect(wrapper.get('[data-testid="phone-login-send"]').attributes('disabled')).toBeUndefined()
  })

  it('submits registration policy, agreement revision, invitation and promo values', async () => {
    sendPhoneCode.mockResolvedValue({ challenge_id: 'challenge', expires_in: 300, retry_after: 60, delivery: 'submitted' })
    loginWithPhone.mockResolvedValue(authResponse())
    const wrapper = mountForm({ invitationCodeEnabled: true, promoCodeEnabled: true })
    await wrapper.get('[data-testid="phone-login-phone"]').setValue('13900000000')
    await wrapper.get('[data-testid="phone-login-send"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="phone-login-code"]').setValue('123456')
    await wrapper.get('[data-testid="phone-login-invitation"]').setValue('INVITE')
    await wrapper.get('[data-testid="phone-login-promo"]').setValue('PROMO')
    await wrapper.get('[data-testid="phone-login-submit"]').trigger('click')
    await flushPromises()

    expect(loginWithPhone).toHaveBeenCalledWith({
      phone: '+8613900000000',
      challenge_id: 'challenge',
      code: '123456',
      register_if_new: true,
      agreement_revision: 'agreement-v3',
      invitation_code: 'INVITE',
      promo_code: 'PROMO'
    })
    expect(wrapper.emitted('authenticated')).toHaveLength(1)
  })

  it('keeps existing phone login available when new registration is disabled', async () => {
    sendPhoneCode.mockResolvedValue({ challenge_id: 'challenge', expires_in: 300, retry_after: 60, delivery: 'submitted' })
    loginWithPhone.mockResolvedValue(authResponse())
    const wrapper = mountForm({ registrationEnabled: false, invitationCodeEnabled: true, promoCodeEnabled: true })
    await wrapper.get('[data-testid="phone-login-phone"]').setValue('13900000000')
    await wrapper.get('[data-testid="phone-login-send"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="phone-login-code"]').setValue('123456')
    await wrapper.get('[data-testid="phone-login-submit"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="phone-registration-disabled"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="phone-login-invitation"]').exists()).toBe(false)
    expect(loginWithPhone).toHaveBeenCalledWith(expect.objectContaining({ register_if_new: false }))
  })

  it('hands a phone login TOTP challenge to the existing modal owner', async () => {
    sendPhoneCode.mockResolvedValue({ challenge_id: 'challenge', expires_in: 300, retry_after: 60, delivery: 'submitted' })
    loginWithPhone.mockResolvedValue({ requires_2fa: true, temp_token: 'temp', user_phone_masked: '+86 139****0000' })
    const wrapper = mountForm()
    await wrapper.get('[data-testid="phone-login-phone"]').setValue('13900000000')
    await wrapper.get('[data-testid="phone-login-send"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="phone-login-code"]').setValue('123456')
    await wrapper.get('[data-testid="phone-login-submit"]').trigger('click')
    await flushPromises()

    expect(wrapper.emitted('requires-2fa')?.[0]?.[0]).toMatchObject({ temp_token: 'temp' })
    expect(wrapper.emitted('authenticated')).toBeUndefined()
  })
})
