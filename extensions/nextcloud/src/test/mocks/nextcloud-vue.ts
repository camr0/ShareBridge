export const NcButton = {
	template: '<button @click="$emit(\'click\')"><slot /></button>',
	emits: ['click'],
}

export const NcModal = {
	props: ['name', 'show'],
	emits: ['close'],
	template: '<div v-if="show !== false" class="nc-modal"><slot /></div>',
}

export const NcTextField = {
	props: ['modelValue', 'label'],
	emits: ['update:modelValue'],
	template: '<input :value="modelValue" @input="$emit(\'update:modelValue\', ($event.target as HTMLInputElement).value)" />',
}

export const NcLoadingIcon = {
	template: '<div class="nc-loading-icon" />',
}

export const NcEmptyContent = {
	props: ['name', 'description'],
	template: '<div class="nc-empty-content"><slot /></div>',
}

export const NcSettingsSection = {
	props: ['name', 'description'],
	template: '<section><slot /></section>',
}

export const NcCheckboxRadioSwitch = {
	props: ['checked'],
	emits: ['update:checked'],
	template: '<input type="checkbox" :checked="checked" @change="$emit(\'update:checked\', !checked)" />',
}

export const NcBadge = {
	template: '<span class="nc-badge"><slot /></span>',
}

export const NcSelect = {
	props: ['modelValue', 'options'],
	emits: ['update:modelValue'],
	template: `
		<select :value="modelValue" @change="$emit('update:modelValue', ($event.target as HTMLSelectElement).value)">
			<option v-for="opt in options" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
		</select>
	`,
}