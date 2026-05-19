// @nuxt/eslint module auto-generates a flat config at .nuxt/eslint.config.mjs
// during `nuxt prepare`. We extend it and add the few project-level overrides
// we care about.
import withNuxt from './.nuxt/eslint.config.mjs'

export default withNuxt(
  {
    rules: {
      // Allow the leading `_` underscore for intentionally-unused args (common
      // in $fetch hook signatures: onRequest({ options }) etc).
      '@typescript-eslint/no-unused-vars': [
        'error',
        {
          argsIgnorePattern: '^_',
          varsIgnorePattern: '^_',
          caughtErrorsIgnorePattern: '^_',
        },
      ],
      // `.d.ts` files describe shapes — empty interfaces are sometimes
      // intentional placeholders we'll flesh out per task.
      '@typescript-eslint/no-empty-object-type': 'off',
    },
  },
  {
    // Ignore generated / build output.
    ignores: [
      '.nuxt/**',
      '.output/**',
      'dist/**',
      'node_modules/**',
      'app/components/ui/**', // shadcn-vue components are owned but generated
    ],
  },
)
