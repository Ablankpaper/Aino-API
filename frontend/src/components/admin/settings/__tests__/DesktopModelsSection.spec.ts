import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import DesktopModelsSection from '../DesktopModelsSection.vue'
import type { DesktopSettings } from '@/api/admin/settings'

const { getAll } = vi.hoisted(() => ({ getAll: vi.fn() }))
vi.mock('@/api/admin/groups', () => ({ getAll }))
vi.mock('vue-i18n', async (original) => ({
  ...(await original<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key })
}))

function settings(): DesktopSettings {
  return { enabled: false, models: [], default_model_id: null, credential_ttl_seconds: 3600 }
}

describe('DesktopModelsSection', () => {
  beforeEach(() => {
    getAll.mockReset().mockResolvedValue([{ id: 17, name: 'Fixture group', platform: 'openai', status: 'active' }])
  })

  it('keeps new capabilities unverified and clears the default when its entry is removed', async () => {
    const wrapper = mount(DesktopModelsSection, { props: { modelValue: settings() } })
    await flushPromises()
    await wrapper.get('[data-testid="desktop-model-add"]').trigger('click')
    const draft = wrapper.emitted('update:modelValue')!.at(-1)![0] as DesktopSettings
    expect(draft.models[0].capabilities).toEqual({ tools: false, vision: false, reasoning: false })
    expect(draft.models[0].agent_verified).toBe(false)
    const selected = { ...draft, default_model_id: 'fixture', models: [{ ...draft.models[0], id: 'fixture', group_id: 17, agent_verified: true, capabilities: { tools: true, vision: false, reasoning: false } }] }
    await wrapper.setProps({ modelValue: selected })
    await wrapper.get('[data-testid="desktop-model-remove"]').trigger('click')
    const removed = wrapper.emitted('update:modelValue')!.at(-1)![0] as DesktopSettings
    expect(removed.models).toEqual([])
    expect(removed.default_model_id).toBeNull()
    expect(selected.models).toHaveLength(1)
  })

  it('keeps configured models on a group load failure and retries only on an explicit action', async () => {
    getAll.mockRejectedValueOnce(new Error('offline'))
    const value: DesktopSettings = { ...settings(), models: [{ id: 'saved', group_id: 17, model: 'fixture', display_name: 'Saved model', provider_label: 'Fixture', platform: 'openai', api_mode: 'responses', sort_order: 0, agent_verified: false, context_window: null, max_output_tokens: null, capabilities: { tools: false, vision: false, reasoning: false } }] }
    const wrapper = mount(DesktopModelsSection, { props: { modelValue: value } })
    await flushPromises()
    expect(getAll).toHaveBeenCalledTimes(1)
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(wrapper.get('[data-testid="desktop-model-name"]').element).toHaveProperty('value', 'Saved model')
    await wrapper.get('[data-testid="desktop-groups-retry"]').trigger('click')
    await flushPromises()
    expect(getAll).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-testid="desktop-groups-retry"]').exists()).toBe(false)
  })
})
