import { cva } from 'class-variance-authority'

export const buttonVariants = cva('button', {
  variants: {
    variant: {
      default: 'button-primary',
      outline: 'button-outline',
      ghost: 'button-ghost',
      danger: 'button-danger',
    },
    size: {
      default: '',
      sm: 'button-sm',
      icon: 'button-icon',
    },
  },
  defaultVariants: { variant: 'default', size: 'default' },
})
