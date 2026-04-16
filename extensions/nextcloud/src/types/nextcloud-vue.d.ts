// Type declaration override for @nextcloud/vue v9
// The package's type declarations have incorrect .ts extensions in export paths
// This file provides correct type declarations for the components we use

declare module '@nextcloud/vue' {
	import { DefineComponent } from 'vue'

	export const NcModal: DefineComponent<{
		name?: string
		show?: boolean
	}, {}, {}>

	export const NcDialog: DefineComponent<{
		name: string
		size?: 'small' | 'normal' | 'large' | 'full'
		open?: boolean
		noClose?: boolean
		closeOnClickOutside?: boolean
	}, {}, {}>

	export const NcButton: DefineComponent<{
		type?: 'primary' | 'secondary' | 'tertiary' | 'error' | 'warning' | 'success'
		disabled?: boolean
	}, {}, {}>

	export const NcTextField: DefineComponent<{
		value?: string
		label?: string
		type?: string
		placeholder?: string
	}, {}, {}>

	export const NcPasswordField: DefineComponent<{
		value?: string
		label?: string
		placeholder?: string
	}, {}, {}>

	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	export const NcSelect: DefineComponent<{
		options?: Array<{ value: number | string; label: string }>
		modelValue?: unknown
		clearable?: boolean
		label?: string
		reduce?: (opt: any) => any
	}, {}, {}>

	export const NcCheckboxRadioSwitch: DefineComponent<{
		checked?: boolean
	}, {}, {}>

	export const NcNoteCard: DefineComponent<{
		type?: 'success' | 'warning' | 'error' | 'info'
	}, {}, {}>

	export const NcLoadingIcon: DefineComponent<{
		size?: number
	}, {}, {}>

	export const NcEmptyContent: DefineComponent<{
		name?: string
		description?: string
	}, {}, {}>

	export const NcSettingsSection: DefineComponent<{
		name?: string
		description?: string
	}, {}, {}>
}