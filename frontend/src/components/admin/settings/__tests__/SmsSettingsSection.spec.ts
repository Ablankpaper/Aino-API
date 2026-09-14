import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import SmsSettingsSection from '@/components/admin/settings/SmsSettingsSection.vue'

const { sendTestSMS, showError, showSuccess } = vi.hoisted(() => ({
  sendTestSMS: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn()
}))

vi.mock('@/api/admin/settings', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/admin/settings')>()),
  sendTestSMS
}))
vi.mock('@/stores', () => ({ useAppStore: () => ({ showError, showSuccess }) }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key })
}))

const settings = {
  enabled: false,
  provider: 'aliyun',
  sign_name: 'Fixture Sign',
  template_code: 'SMS_FIXTURE',
  template_params: { code: 'code', minutes: 'ttl_minutes' },
  template_verified: true,
  code_length: 6,
  ttl_seconds: 300,
  cooldown_seconds: 60,
  max_attempts: 5,
  phone_hour_limit: 5,
  phone_day_limit: 10,
  ip_hour_limit: 30,
  global_day_limit: 1000,
  credentials_configured: true,
  hmac_configured: true,
  ready: false,
  reason_code: 'SMS_DISABLED'
}

describe('SmsSettingsSection', () => {
  beforeEach(() => {
    sendTestSMS.mockReset()
    showError.mockReset()
    showSuccess.mockReset()
  })

  it('shows readiness booleans without rendering any credential value', () => {
    const wrapper = mount(SmsSettingsSection, { props: { modelValue: { ...settings, enabled: true, ready: true } } })

    expect(wrapper.get('[data-testid="sms-credentials-status"]').text()).toContain('admin.settings.sms.configured')
    expect(wrapper.get('[data-testid="sms-hmac-status"]').text()).toContain('admin.settings.sms.configured')
    expect(wrapper.find('input[type="password"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('fixture-access-secret')
  })

  it('emits editable policy changes and keeps the entered value after a failed action', async () => {
    sendTestSMS.mockRejectedValue({ message: 'fixture send failed' })
    const wrapper = mount(SmsSettingsSection, { props: { modelValue: { ...settings, enabled: true, ready: true } } })

    await wrapper.get('[data-testid="sms-sign-name"]').setValue('Edited Sign')
    const update = wrapper.emitted('update:modelValue')?.at(-1)?.[0] as Record<string, unknown>
    expect(update.sign_name).toBe('Edited Sign')

    await wrapper.get('[data-testid="sms-test-phone"]').setValue('13900000000')
    await wrapper.get('[data-testid="sms-test-send"]').trigger('click')
    await flushPromises()
    expect((wrapper.get('[data-testid="sms-test-phone"]').element as HTMLInputElement).value).toBe('13900000000')
    expect(showError).toHaveBeenCalledWith('fixture send failed')
  })

  it('never sends on mount and requires an explicit receiver click', async () => {
    sendTestSMS.mockResolvedValue({ delivery: 'submitted', expires_in: 300, retry_after: 60 })
    const wrapper = mount(SmsSettingsSection, { props: { modelValue: { ...settings, enabled: true, ready: true } } })
    expect(sendTestSMS).not.toHaveBeenCalled()
    expect(wrapper.get('[data-testid="sms-test-send"]').attributes('disabled')).toBeDefined()

    await wrapper.get('[data-testid="sms-test-phone"]').setValue('+86 139 0000 0000')
    await wrapper.get('[data-testid="sms-test-send"]').trigger('click')
    await flushPromises()

    expect(sendTestSMS).toHaveBeenCalledWith({ phone: '+8613900000000' })
  })
})
