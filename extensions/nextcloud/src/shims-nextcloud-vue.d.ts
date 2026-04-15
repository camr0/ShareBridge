declare module '@nextcloud/vue' {
    import { DefineComponent } from 'vue'

    export const NcButton: DefineComponent<{
        type?: 'primary' | 'secondary' | 'tertiary' | 'tertiary-no-background' | 'error' | 'warning' | 'success'
        nativeType?: 'submit' | 'reset' | 'button'
        disabled?: boolean
        loading?: boolean
        wide?: boolean
    }>
    export const NcCheckboxRadioSwitch: DefineComponent<{
        checked?: boolean
        disabled?: boolean
    }>
    export const NcInputField: DefineComponent<{
        label?: string
        value?: string
        type?: string
        disabled?: boolean
        placeholder?: string
    }>
    export const NcPasswordField: DefineComponent<{
        label?: string
        value?: string
        disabled?: boolean
    }>
    export const NcTextField: DefineComponent<{
        label?: string
        value?: string
        disabled?: boolean
        placeholder?: string
    }>
    export const NcModal: DefineComponent<{
        show?: boolean
        size?: 'small' | 'normal' | 'large' | 'full'
        canClose?: boolean
    }>
    export const NcActionButton: DefineComponent<{
        icon?: string
        disabled?: boolean
    }>
    export const NcActions: DefineComponent
    export const NcEmptyContent: DefineComponent<{
        name?: string
        description?: string
    }>
    export const NcLoadingIcon: DefineComponent<{
        size?: number
    }>
    export const NcDateTime: DefineComponent
    export const NcPopover: DefineComponent
    export const NcSettingsSection: DefineComponent<{
        name?: string
        description?: string
    }>
}