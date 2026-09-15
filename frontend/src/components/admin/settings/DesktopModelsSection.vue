<template>
  <section class="card" aria-labelledby="desktop-models-title">
    <div class="flex items-start justify-between gap-4 border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <div>
        <h2 id="desktop-models-title" class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('admin.settings.desktop.title') }}</h2>
        <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.settings.desktop.description') }}</p>
      </div>
      <Toggle :model-value="modelValue.enabled" :aria-label="t('admin.settings.desktop.enabled')" @update:model-value="update({ enabled: $event })" />
    </div>
    <div class="space-y-5 p-6">
      <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.settings.desktop.pricingHint') }}</p>
      <div class="grid gap-4 md:grid-cols-2">
        <div class="space-y-1 text-sm text-gray-700 dark:text-gray-300">
          <label for="desktop-default-model">{{ t('admin.settings.desktop.defaultModel') }}</label>
          <Select id="desktop-default-model" :model-value="modelValue.default_model_id" :options="defaultOptions" :aria-label="t('admin.settings.desktop.defaultModel')" @update:model-value="update({ default_model_id: $event === null ? null : String($event) })" />
        </div>
        <label class="space-y-1 text-sm text-gray-700 dark:text-gray-300">
          <span>{{ t('admin.settings.desktop.credentialTTL') }}</span>
          <input class="input" type="number" min="300" max="3600" step="1" required :value="modelValue.credential_ttl_seconds" @input="update({ credential_ttl_seconds: numberValue($event) })" />
        </label>
      </div>
      <div v-if="groupsError" role="alert" class="flex flex-wrap items-center gap-3 text-sm text-red-600 dark:text-red-400">
        <span>{{ t('admin.settings.desktop.groupsFailed') }}</span>
        <button type="button" class="btn btn-secondary" data-testid="desktop-groups-retry" @click="loadGroups">{{ t('common.refresh') }}</button>
      </div>
      <p v-if="!modelValue.models.length" class="rounded-lg border border-dashed border-gray-200 p-5 text-sm text-gray-500 dark:border-dark-600 dark:text-gray-400">{{ t('admin.settings.desktop.empty') }}</p>
      <div v-for="(entry, index) in modelValue.models" :key="index" class="space-y-4 rounded-xl border border-gray-200 p-4 dark:border-dark-600">
        <div class="flex items-center justify-between gap-3">
          <h3 class="min-w-0 truncate font-medium text-gray-900 dark:text-white">{{ entry.display_name || t('admin.settings.desktop.newModel') }}</h3>
          <button type="button" class="btn btn-secondary shrink-0" data-testid="desktop-model-remove" @click="remove(index)">{{ t('common.delete') }}</button>
        </div>
        <div class="grid gap-4 md:grid-cols-2">
          <label v-for="field in textFields" :key="field" class="space-y-1 text-sm text-gray-700 dark:text-gray-300">
            <span>{{ t(`admin.settings.desktop.${field}`) }}</span>
            <input class="input" :data-testid="field === 'display_name' ? 'desktop-model-name' : undefined" :value="entry[field]" :maxlength="field === 'id' ? 128 : 256" :pattern="field === 'id' ? '[a-zA-Z0-9][a-zA-Z0-9._-]*' : undefined" required @input="edit(index, { [field]: textValue($event) })" />
          </label>
          <div class="space-y-1 text-sm text-gray-700 dark:text-gray-300">
            <label :for="`desktop-group-${index}`">{{ t('admin.settings.desktop.group') }}</label>
            <Select :id="`desktop-group-${index}`" :model-value="entry.group_id || null" :options="groupOptions(entry)" :disabled="groupsLoading || groupsError" searchable :aria-label="t('admin.settings.desktop.group')" @update:model-value="selectGroup(index, Number($event))" />
          </div>
          <div class="space-y-1 text-sm text-gray-700 dark:text-gray-300">
            <label :for="`desktop-platform-${index}`">{{ t('admin.settings.desktop.platform') }}</label>
            <Select :id="`desktop-platform-${index}`" :model-value="entry.platform" :options="platformOptions" :disabled="groups.find(group => group.id === entry.group_id)?.platform !== 'composite'" :aria-label="t('admin.settings.desktop.platform')" @update:model-value="edit(index, { platform: $event as DesktopModelEntry['platform'] })" />
          </div>
          <div class="space-y-1 text-sm text-gray-700 dark:text-gray-300">
            <label :for="`desktop-protocol-${index}`">{{ t('admin.settings.desktop.protocol') }}</label>
            <Select :id="`desktop-protocol-${index}`" :model-value="entry.api_mode" :options="protocolOptions" :aria-label="t('admin.settings.desktop.protocol')" @update:model-value="edit(index, { api_mode: $event as DesktopModelEntry['api_mode'] })" />
          </div>
          <label class="space-y-1 text-sm text-gray-700 dark:text-gray-300">
            <span>{{ t('admin.settings.desktop.sortOrder') }}</span>
            <input class="input" type="number" step="1" :value="entry.sort_order" @input="edit(index, { sort_order: numberValue($event) })" />
          </label>
          <label v-for="field in limitFields" :key="field" class="space-y-1 text-sm text-gray-700 dark:text-gray-300">
            <span>{{ t(`admin.settings.desktop.${field}`) }}</span>
            <input class="input" type="number" min="1" step="1" :value="entry[field]" :placeholder="t('admin.settings.desktop.unknown')" @input="edit(index, { [field]: textValue($event) === '' ? null : numberValue($event) })" />
          </label>
        </div>
        <div class="flex flex-wrap gap-x-6 gap-y-3 text-sm text-gray-700 dark:text-gray-300">
          <label v-for="capability in capabilityFields" :key="capability" class="flex items-center gap-2">
            <input type="checkbox" :checked="entry.capabilities[capability]" @change="edit(index, { capabilities: { ...entry.capabilities, [capability]: checkedValue($event) } })" />
            {{ t(`admin.settings.desktop.${capability}`) }}
          </label>
        </div>
        <label class="flex items-start gap-2 text-sm text-gray-700 dark:text-gray-300">
          <input type="checkbox" class="mt-1" :checked="entry.agent_verified" @change="edit(index, { agent_verified: checkedValue($event) })" />
          <span>{{ t('admin.settings.desktop.verified') }}</span>
        </label>
      </div>
      <button type="button" class="btn btn-secondary" data-testid="desktop-model-add" :disabled="modelValue.models.length >= 200" @click="add">{{ t('admin.settings.desktop.add') }}</button>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Select from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'
import { getAll } from '@/api/admin/groups'
import type { DesktopModelEntry, DesktopSettings } from '@/api/admin/settings'
import type { AdminGroup } from '@/types'

const props = defineProps<{ modelValue: DesktopSettings }>()
const emit = defineEmits<{ 'update:modelValue': [value: DesktopSettings] }>()
const { t } = useI18n()
const groups = ref<AdminGroup[]>([])
const groupsLoading = ref(false)
const groupsError = ref(false)
const textFields = ['id', 'display_name', 'model', 'provider_label'] as const
const limitFields = ['context_window', 'max_output_tokens'] as const
const capabilityFields = ['tools', 'vision', 'reasoning'] as const
const platforms: DesktopModelEntry['platform'][] = ['openai', 'anthropic', 'gemini', 'antigravity', 'grok', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go']
const platformOptions = platforms.map(value => ({ value, label: value }))
const protocolOptions = [
  { value: 'chat_completions', label: 'Chat Completions' },
  { value: 'responses', label: 'Responses' },
  { value: 'anthropic_messages', label: 'Anthropic Messages' }
]
const defaultOptions = computed(() => [
  { value: null, label: t('admin.settings.desktop.noDefault') },
  ...props.modelValue.models.filter(entry => entry.id && entry.agent_verified && entry.capabilities.tools).map(entry => ({ value: entry.id, label: entry.display_name || entry.id }))
])

function update(patch: Partial<DesktopSettings>): void {
  emit('update:modelValue', { ...props.modelValue, ...patch })
}

function edit(index: number, patch: Partial<DesktopModelEntry>): void {
  const previous = props.modelValue.models[index]
  const entry = { ...previous, ...patch }
  // Changing the upstream selection invalidates its prior end-to-end verification.
  if (['model', 'group_id', 'platform', 'api_mode'].some(key => key in patch && patch[key as keyof DesktopModelEntry] !== previous[key as keyof DesktopModelEntry])) entry.agent_verified = false
  const models = props.modelValue.models.map((model, i) => i === index ? entry : model)
  let defaultID = props.modelValue.default_model_id
  if (defaultID === previous.id) defaultID = entry.agent_verified && entry.capabilities.tools ? entry.id || null : null
  update({ models, default_model_id: defaultID })
}

function add(): void {
  update({ models: [...props.modelValue.models, { id: '', group_id: 0, model: '', display_name: '', provider_label: '', platform: 'openai', api_mode: 'chat_completions', sort_order: props.modelValue.models.length, agent_verified: false, capabilities: { tools: false, vision: false, reasoning: false }, context_window: null, max_output_tokens: null }] })
}

function remove(index: number): void {
  update({ models: props.modelValue.models.filter((_, i) => i !== index), default_model_id: props.modelValue.default_model_id === props.modelValue.models[index].id ? null : props.modelValue.default_model_id })
}

function groupOptions(entry: DesktopModelEntry) {
  const options = groups.value.map(group => ({ value: group.id, label: `${group.name} · ${group.platform}` }))
  if (entry.group_id && !groups.value.some(group => group.id === entry.group_id)) options.push({ value: entry.group_id, label: `${t('admin.settings.desktop.unavailableGroup')} #${entry.group_id}` })
  return options
}

function selectGroup(index: number, groupID: number): void {
  const group = groups.value.find(item => item.id === groupID)
  if (!group) return
  edit(index, { group_id: groupID, ...(group.platform !== 'composite' ? { platform: group.platform } : {}) })
}

function textValue(event: Event): string { return (event.target as HTMLInputElement).value }
function numberValue(event: Event): number { return Number(textValue(event)) }
function checkedValue(event: Event): boolean { return (event.target as HTMLInputElement).checked }

async function loadGroups(): Promise<void> {
  groupsLoading.value = true
  groupsError.value = false
  try { groups.value = await getAll() }
  catch { groupsError.value = true }
  finally { groupsLoading.value = false }
}

onMounted(loadGroups)
</script>
