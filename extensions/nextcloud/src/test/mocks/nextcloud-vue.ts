import type { DefineComponent } from 'vue'
import { ref } from 'vue'

export const NcButton: DefineComponent = {
	template: '<button @click="$emit(\'click\')"><slot /></button>',
	emits: ['click'],
} as unknown as DefineComponent

export const NcModal: DefineComponent = {
	props: ['name', 'show'],
	emits: ['close'],
	template: '<div v-if="show !== false" class="nc-modal" :data-testid="$attrs[\'data-testid\']"><slot /></div>',
} as unknown as DefineComponent

export const NcDialog: DefineComponent = {
	props: ['name', 'size', 'open'],
	emits: ['closing', 'update:open'],
	template: '<div v-if="open !== false" class="nc-dialog"><slot /><div class="nc-dialog__actions"><slot name="actions" /></div></div>',
} as unknown as DefineComponent

export const NcTextField: DefineComponent = {
	props: ['value', 'label', 'type', 'placeholder'],
	emits: ['update:value'],
	template: '<input :value="value" @input="$emit(\'update:value\', $event.target.value)" :data-testid="$attrs[\'data-testid\']" />',
} as unknown as DefineComponent

export const NcPasswordField: DefineComponent = {
	props: ['value', 'label', 'placeholder'],
	emits: ['update:value'],
	template: '<input type="password" :value="value" @input="$emit(\'update:value\', $event.target.value)" :data-testid="$attrs[\'data-testid\']" />',
} as unknown as DefineComponent

export const NcLoadingIcon: DefineComponent = {
	props: ['size'],
	template: '<div class="nc-loading-icon" />',
} as unknown as DefineComponent

export const NcEmptyContent: DefineComponent = {
	props: ['name', 'description'],
	template: '<div class="nc-empty-content" :data-testid="$attrs[\'data-testid\']"><p class="nc-empty-content__name">{{ name }}</p><p class="nc-empty-content__desc">{{ description }}</p></div>',
} as unknown as DefineComponent

export const NcSettingsSection: DefineComponent = {
	props: ['name', 'description'],
	template: '<section class="nc-settings-section"><slot /></section>',
} as unknown as DefineComponent

export const NcCheckboxRadioSwitch: DefineComponent = {
	props: { checked: { type: Boolean, default: false } },
	emits: ['update:checked'],
	setup(props: { checked: boolean }) {
		const localChecked = ref(props.checked)
		const toggle = () => {
			localChecked.value = !localChecked.value
		}
		return { localChecked, toggle }
	},
	template: '<label class="nc-checkbox"><input type="checkbox" :checked="localChecked" @change="toggle" :data-testid="$attrs[\'data-testid\']" /><slot /></label>',
} as unknown as DefineComponent

export const NcNoteCard: DefineComponent = {
	props: ['type'],
	template: '<div class="nc-note-card" :data-testid="$attrs[\'data-testid\']"><slot /></div>',
} as unknown as DefineComponent

export const NcSelect: DefineComponent = {
	props: ['options', 'modelValue', 'clearable', 'label', 'reduce'],
	emits: ['update:modelValue'],
	template: `
		<div class="nc-select-wrapper" :data-testid="$attrs['data-testid']">
			<select>
				<option v-for="opt in options" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
			</select>
		</div>
	`,
} as unknown as DefineComponent