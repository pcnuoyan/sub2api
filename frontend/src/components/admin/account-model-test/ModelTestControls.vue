<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { ModelTestRequest, ModelTestPromptKey, ModelTestReasoning } from '@/api/admin/accountModelTest'
import { CANDY_PROMPT, PELICAN_PROMPT, MODEL_TEST_REASONING, MODEL_TEST_MAX_PROMPT_BYTES, isTextTestModel } from '@/utils/accountModelTest'

type TestDraft = Omit<ModelTestRequest, 'account_id'>
const draft = defineModel<TestDraft>({ required: true })
defineProps<{ models: string[] }>()
const { t } = useI18n()
const modelError = computed(() => draft.value.model_id.trim() && !isTextTestModel(draft.value.model_id))
const promptTooLong = computed(() => new TextEncoder().encode(draft.value.prompt.trim()).length > MODEL_TEST_MAX_PROMPT_BYTES)

function setModel(event: Event) {
  draft.value = { ...draft.value, model_id: (event.target as HTMLInputElement).value }
}

function setReasoning(event: Event) {
  draft.value = { ...draft.value, reasoning_effort: (event.target as HTMLSelectElement).value as ModelTestReasoning }
}

function setPreset(event: Event) {
  const key = (event.target as HTMLSelectElement).value as ModelTestPromptKey
  draft.value = { ...draft.value, prompt_key: key, prompt: key === 'candy' ? CANDY_PROMPT : key === 'pelican' ? PELICAN_PROMPT : draft.value.prompt }
}

function setPrompt(event: Event) {
  draft.value = { ...draft.value, prompt: (event.target as HTMLTextAreaElement).value, prompt_key: 'custom' }
}
</script>

<template>
  <section class="card space-y-4 p-4 sm:p-5" :aria-label="t('admin.accountModelTest.prompt')">
    <div class="grid gap-4 md:grid-cols-3">
      <div>
        <label for="model-test-model" class="mb-1.5 block text-sm font-medium">{{ t('admin.accountModelTest.model') }}</label>
        <input id="model-test-model" :value="draft.model_id" list="model-test-models" class="input" autocomplete="off" maxlength="200" :aria-invalid="!!modelError" @input="setModel" />
        <datalist id="model-test-models"><option v-for="model in models" :key="model" :value="model" /></datalist>
        <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accountModelTest.modelHint') }}</p>
      </div>
      <div>
        <label for="model-test-reasoning" class="mb-1.5 block text-sm font-medium">{{ t('admin.accountModelTest.reasoning') }}</label>
        <select id="model-test-reasoning" :value="draft.reasoning_effort" class="input" @change="setReasoning">
          <option v-for="effort in MODEL_TEST_REASONING" :key="effort" :value="effort">{{ effort || t('admin.accountModelTest.modelDefault') }}</option>
        </select>
      </div>
      <div>
        <label for="model-test-preset" class="mb-1.5 block text-sm font-medium">{{ t('admin.accountModelTest.prompt') }}</label>
        <select id="model-test-preset" :value="draft.prompt_key" class="input" @change="setPreset">
          <option value="candy">{{ t('admin.accountModelTest.candy') }}</option>
          <option value="pelican">{{ t('admin.accountModelTest.pelican') }}</option>
          <option value="custom">{{ t('admin.accountModelTest.custom') }}</option>
        </select>
      </div>
    </div>
    <div>
      <label for="model-test-prompt" class="mb-1.5 block text-sm font-medium">{{ t('admin.accountModelTest.promptContent') }}</label>
      <textarea id="model-test-prompt" :value="draft.prompt" class="input min-h-24 resize-y" rows="3" maxlength="16000" @input="setPrompt" />
      <p v-if="draft.prompt_key === 'candy'" class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accountModelTest.candyHint') }}</p>
      <p v-if="promptTooLong" role="alert" class="mt-1.5 text-xs text-red-600 dark:text-red-400">{{ t('admin.accountModelTest.invalidPrompt') }}</p>
    </div>
    <p v-if="modelError" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ t('admin.accountModelTest.textOnly') }}</p>
    <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accountModelTest.costNotice') }}</p>
  </section>
</template>
