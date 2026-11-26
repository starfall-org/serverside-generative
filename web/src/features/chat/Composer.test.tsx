import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { Composer } from './Composer'
import { ToastProvider } from '../../components/ui/toast'

const mocks = vi.hoisted(() => ({
  state: {
    draft: '',
    run: null as null | { phase: string },
    maxRunBytes: 64 << 10,
  },
  send: vi.fn(async () => {}),
  setDraft: vi.fn(),
  cancel: vi.fn(async () => {}),
}))

vi.mock('./ChatProvider', () => ({
  useChat: () => ({
    active: {
      draft: mocks.state.draft,
      run: mocks.state.run,
      instructions: '',
    },
    activeId: 'chat-1',
    selection: { modelId: 'model-1' },
    setDraft: mocks.setDraft,
    send: mocks.send,
    cancel: mocks.cancel,
    maxRunBytes: mocks.state.maxRunBytes,
  }),
}))

vi.mock('../../app/preferences', () => ({
  usePreferences: () => ({ t: (key: string) => key }),
}))

function renderComposer() {
  return render(
    <ToastProvider>
      <Composer onOpenModel={() => {}} />
    </ToastProvider>,
  )
}

describe('Composer', () => {
  beforeEach(() => {
    mocks.state.draft = ''
    mocks.state.run = null
    mocks.state.maxRunBytes = 64 << 10
    mocks.send.mockClear()
    mocks.setDraft.mockClear()
    mocks.cancel.mockClear()
  })

  it('sends on Enter and keeps Shift+Enter for a new line', async () => {
    mocks.state.draft = 'Hello'
    renderComposer()
    const textarea = screen.getByLabelText('chat.composerPlaceholder')

    fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: true })
    expect(mocks.send).not.toHaveBeenCalled()

    fireEvent.keyDown(textarea, { key: 'Enter' })
    await waitFor(() => expect(mocks.send).toHaveBeenCalledWith('Hello'))
  })

  it('does not submit while an IME composition is active', () => {
    mocks.state.draft = 'こんにちは'
    renderComposer()
    const textarea = screen.getByLabelText('chat.composerPlaceholder')

    fireEvent.compositionStart(textarea)
    fireEvent.keyDown(textarea, { key: 'Enter' })
    expect(mocks.send).not.toHaveBeenCalled()
  })

  it('blocks oversized messages and shows the byte budget', () => {
    mocks.state.draft = '😀😀😀'
    mocks.state.maxRunBytes = 10
    renderComposer()

    expect(screen.getByText('chat.tooLarge')).toBeInTheDocument()
    expect(screen.getByLabelText('chat.send')).toBeDisabled()
  })

  it('switches send to stop while a run is active', () => {
    mocks.state.draft = 'Hello'
    mocks.state.run = { phase: 'streaming' }
    renderComposer()

    const stop = screen.getByLabelText('chat.stop')
    expect(stop).toBeEnabled()
    fireEvent.click(stop)
    expect(mocks.cancel).toHaveBeenCalledTimes(1)
  })
})
