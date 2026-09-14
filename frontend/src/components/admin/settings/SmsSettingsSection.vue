<template>
  <div class="card">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('admin.settings.sms.title') }}</h2>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.settings.sms.description') }}</p>
        </div>
        <Toggle :model-value="modelValue.enabled" @update:model-value="update({ enabled: $event })" />
      </div>
    </div>

    <div class="space-y-5 p-6">
      <div class="grid gap-3 sm:grid-cols-3">
        <p data-testid="sms-credentials-status" class="text-sm text-gray-600 dark:text-gray-300">
          {{ t('admin.settings.sms.credentials') }}: {{ t(modelValue.credentials_configured ? 'admin.settings.sms.configured' : 'admin.settings.sms.notConfigured') }}
        </p>
        <p data-testid="sms-hmac-status" class="text-sm text-gray-600 dark:text-gray-300">
          {{ t('admin.settings.sms.hmac') }}: {{ t(modelValue.hmac_configured ? 'admin.settings.sms.configured' : 'admin.settings.sms.notConfigured') }}
        </p>
        <p data-testid="sms-ready-status" class="text-sm text-gray-600 dark:text-gray-300">
          {{ t('admin.settings.sms.readiness') }}: {{ t(modelValue.ready ? 'admin.settings.sms.ready' : 'admin.settings.sms.notReady') }}
        </p>
      </div>

      <div class="grid gap-4 md:grid-cols-2">
        <label class="space-y-1 text-sm text-gray-700 dark:text-gray-300">
          <span>{{ t('admin.settings.sms.provider') }}</span>
          <select class="input" :value="modelValue.provider" @change="updateString('provider', $event)">
            <option value="aliyun">Aliyun</option>
          </select>
        </label>
        <label class="space-y-1 text-sm text-gray-700 dark:text-gray-300">
          <span>{{ t('admin.settings.sms.signName') }}</span>
          <input data-testid="sms-sign-name" class="input" :value="modelValue.sign_name" @input="updateString('sign_name', $event)" />
        </label>
        <label class="space-y-1 text-sm text-gray-700 dark:text-gray-300">
          <span>{{ t('admin.settings.sms.templateCode') }}</span>
          <input class="input" :value="modelValue.template_code" @input="updateString('template_code', $event)" />
        </label>
        <label class="flex items-center gap-3 text-sm text-gray-700 dark:text-gray-300">
          <input type="checkbox" :checked="modelValue.template_verified" @change="updateBoolean('template_verified', $event)" />
          <span>{{ t('admin.settings.sms.templateVerified') }}</span>
        </label>
      </div>

      <label class="block space-y-1 text-sm text-gray-700 dark:text-gray-300">
        <span>{{ t('admin.settings.sms.templateParams') }}</span>
        <textarea v-model="templateParamsJSON" class="input min-h-24 font-mono text-sm" @blur="commitTemplateParams"></textarea>
        <span v-if="templateParamsError" class="text-xs text-red-600">{{ t('admin.settings.sms.invalidTemplateParams') }}</span>
      </label>

      <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <label v-for="field in numericFields" :key="field.key" class="space-y-1 text-sm text-gray-700 dark:text-gray-300">
          <span>{{ t(field.label) }}</span>
          <input type="number" class="input" :min="field.min" :max="field.max" :value="modelValue[field.key]" @input="updateNumber(field.key, $event)" />
        </label>
      </div>

      <div class="border-t border-gray-100 pt-5 dark:border-dark-700">
        <h3 class="text-sm font-medium text-gray-900 dark:text-white">{{ t('admin.settings.sms.testTitle') }}</h3>
        <div class="mt-3 flex flex-col gap-2 sm:flex-row">
          <input v-model="testPhone" data-testid="sms-test-phone" type="tel" inputmode="tel" autocomplete="off" class="input flex-1" :placeholder="t('admin.settings.sms.testPhone')" />
          <button data-testid="sms-test-send" type="button" class="btn btn-secondary" :disabled="testSending || !modelValue.ready || !normalizedTestPhone" @click="sendTest">
            {{ testSending ? t('common.sending') : t('admin.settings.sms.sendTest') }}
          </button>
        </div>
      </div>
    </div>
  </div>
  <TotpStepUpDialog :controller="stepUp" />
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { sendTestSMS, type SMSEditableSettings, type SMSSettings } from '@/api/admin/settings'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import Toggle from '@/components/common/Toggle.vue'
import { isStepUpCancelled, useStepUp } from '@/composables/useStepUp'
import { useAppStore } from '@/stores'
import { extractApiErrorMessage } from '@/utils/apiError'
import { normalizeCNPhone, phoneDeliveryMessageKey } from '@/utils/phone'

const props = defineProps<{ modelValue: SMSSettings }>()
const emit = defineEmits<{ 'update:modelValue': [value: SMSSettings] }>()
const { t } = useI18n()
const appStore = useAppStore()
const stepUp = useStepUp()
const testPhone = ref('')
const testSending = ref(false)
const templateParamsJSON = ref(JSON.stringify(props.modelValue.template_params, null, 2))
const templateParamsError = ref(false)
const normalizedTestPhone = computed(() => normalizeCNPhone(testPhone.value))

type NumericKey = keyof Pick<SMSEditableSettings, 'code_length' | 'ttl_seconds' | 'cooldown_seconds' | 'max_attempts' | 'phone_hour_limit' | 'phone_day_limit' | 'ip_hour_limit' | 'global_day_limit'>
const numericFields: Array<{ key: NumericKey; label: string; min: number; max?: number }> = [
  { key: 'code_length', label: 'admin.settings.sms.codeLength', min: 4, max: 8 },
  { key: 'ttl_seconds', label: 'admin.settings.sms.ttlSeconds', min: 30, max: 1800 },
  { key: 'cooldown_seconds', label: 'admin.settings.sms.cooldownSeconds', min: 1, max: 3600 },
  { key: 'max_attempts', label: 'admin.settings.sms.maxAttempts', min: 1, max: 20 },
  { key: 'phone_hour_limit', label: 'admin.settings.sms.phoneHourLimit', min: 1 },
  { key: 'phone_day_limit', label: 'admin.settings.sms.phoneDayLimit', min: 1 },
  { key: 'ip_hour_limit', label: 'admin.settings.sms.ipHourLimit', min: 1 },
  { key: 'global_day_limit', label: 'admin.settings.sms.globalDayLimit', min: 1 }
]

watch(() => props.modelValue.template_params, (value) => {
  if (!templateParamsError.value) templateParamsJSON.value = JSON.stringify(value, null, 2)
}, { deep: true })

function update(patch: Partial<SMSEditableSettings>): void {
  emit('update:modelValue', { ...props.modelValue, ...patch })
}

function updateString(key: 'provider' | 'sign_name' | 'template_code', event: Event): void {
  update({ [key]: (event.target as HTMLInputElement).value })
}

function updateBoolean(key: 'template_verified', event: Event): void {
  update({ [key]: (event.target as HTMLInputElement).checked })
}

function updateNumber(key: NumericKey, event: Event): void {
  update({ [key]: Number((event.target as HTMLInputElement).value) })
}

function commitTemplateParams(): void {
  try {
    const parsed = JSON.parse(templateParamsJSON.value) as unknown
    if (!parsed || Array.isArray(parsed) || typeof parsed !== 'object' || Object.values(parsed).some((value) => typeof value !== 'string')) throw new Error('invalid')
    templateParamsError.value = false
    update({ template_params: parsed as Record<string, string> })
  } catch {
    templateParamsError.value = true
  }
}

async function sendTest(): Promise<void> {
  if (!normalizedTestPhone.value) return
  testSending.value = true
  try {
    const result = await stepUp.run(() => sendTestSMS({ phone: normalizedTestPhone.value! }))
    appStore.showSuccess(t('admin.settings.sms.testSubmitted', { delivery: t(phoneDeliveryMessageKey(result.delivery)) }))
  } catch (error) {
    if (!isStepUpCancelled(error)) appStore.showError(extractApiErrorMessage(error, t('admin.settings.sms.testFailed')))
  } finally {
    testSending.value = false
  }
}
</script>
