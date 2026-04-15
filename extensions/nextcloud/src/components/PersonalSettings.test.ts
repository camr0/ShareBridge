import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { setActivePinia, createPinia } from 'pinia'
import { useSettingsStore } from '../stores/settings'
import PersonalSettings from './PersonalSettings.vue'

describe('PersonalSettings', () => {
    beforeEach(() => {
        setActivePinia(createPinia())
        // Pre-populate store so fetchSettings no-ops
        const store = useSettingsStore()
        store.$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })
    })

    it('shows loading state while settings are being fetched', () => {
        const store = useSettingsStore()
        store.$patch({ loaded: false, loading: true })

        const wrapper = mount(PersonalSettings)
        expect(wrapper.find('.sb-settings-loading').exists()).toBe(true)
        expect(wrapper.text()).toContain('Loading')
    })

    it('renders agentUrl and apiKey fields when loaded', () => {
        const wrapper = mount(PersonalSettings)
        const inputs = wrapper.findAll('input')
        expect(inputs.length).toBeGreaterThanOrEqual(2)
    })

    it('shows current agentUrl in the agent URL field', () => {
        const wrapper = mount(PersonalSettings)
        const agentUrlInput = wrapper.find('[data-testid="agent-url-input"]')
        expect((agentUrlInput.element as HTMLInputElement).value).toBe('http://localhost:7878')
    })

    it('shows current apiKey in the API key field', () => {
        const wrapper = mount(PersonalSettings)
        const apiKeyInput = wrapper.find('[data-testid="api-key-input"]')
        expect((apiKeyInput.element as HTMLInputElement).value).toBe('sb_key')
    })

    it('Save button calls saveSettings with updated values', async () => {
        const store = useSettingsStore()
        const saveSpy = vi.spyOn(store, 'saveSettings').mockResolvedValue(undefined)

        const wrapper = mount(PersonalSettings)
        const agentInput = wrapper.find('[data-testid="agent-url-input"]')
        await agentInput.setValue('http://newhost:7878')

        await wrapper.find('[data-testid="save-btn"]').trigger('click')
        await flushPromises()

        expect(saveSpy).toHaveBeenCalledOnce()
        expect(store.agentUrl).toBe('http://newhost:7878')
    })

    it('shows a success message after saving', async () => {
        const store = useSettingsStore()
        vi.spyOn(store, 'saveSettings').mockResolvedValue(undefined)

        const wrapper = mount(PersonalSettings)
        await wrapper.find('[data-testid="save-btn"]').trigger('click')
        await flushPromises()

        expect(wrapper.text()).toContain('Saved')
    })
})
