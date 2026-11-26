import { describe, expect, it } from 'vitest'
import {
  parseCapabilities,
  parseConversation,
  parseEvent,
  parseListPage,
  parseProviderCatalog,
  parseSessionView,
} from './contracts'

const conversation = {
  id: 'chat-1',
  title: 'Example',
  created_at: 1,
  updated_at: 2,
  messages: [
    {
      id: 'm1',
      role: 'user',
      versions: [{ content: 'Hello' }],
      active_version_index: 0,
    },
  ],
}

describe('runtime contracts', () => {
  it('parses a conversation and clamps invalid active versions', () => {
    const parsed = parseConversation({
      ...conversation,
      messages: [
        { ...conversation.messages[0], active_version_index: 9 },
        {
          id: 'm2',
          role: 'model',
          versions: [{ content: null }, { content: 'kept' }],
          active_version_index: 1,
        },
      ],
    })
    expect(parsed?.messages[0]?.active_version_index).toBe(0)
    expect(parsed?.messages[1]?.versions[0]?.content).toBe('')
    expect(parsed?.messages[1]?.versions[1]?.content).toBe('kept')
  })

  it('rejects duplicate IDs, invalid roles and malformed envelopes', () => {
    expect(
      parseConversation({
        ...conversation,
        messages: [conversation.messages[0], conversation.messages[0]],
      }),
    ).toBeNull()
    expect(
      parseConversation({
        ...conversation,
        messages: [{ ...conversation.messages[0], role: 'tool' }],
      }),
    ).toBeNull()
    expect(parseConversation({ ...conversation, messages: 'nope' })).toBeNull()
    expect(parseConversation({ ...conversation, title: undefined })).toBeNull()
  })

  it('parses list pages, tombstones and forward-compatible fields', () => {
    const page = parseListPage({
      version: 1,
      items: [
        { id: 'a', revision: 2, title: 'A', deleted_at: 9 },
        { id: 'b', revision: 'bad', future: true },
        { nope: true },
      ],
      next_cursor: 'cursor-2',
    })
    expect(page.items).toHaveLength(2)
    expect(page.items[0]).toMatchObject({ id: 'a', revision: 2, deleted_at: 9 })
    expect(page.items[1]).toMatchObject({ id: 'b', revision: 0, future: true })
    expect(page.next_cursor).toBe('cursor-2')
  })

  it('parses capabilities and provider catalogs defensively', () => {
    expect(
      parseCapabilities({
        protocol: 5,
        transports: ['http'],
        operations: ['session.open'],
        chat_features: { negotiation: { generation: { max_run_content_bytes: 65536 } } },
      }),
    ).toMatchObject({ protocol: 5, transports: ['http'] })
    expect(parseCapabilities({ transports: [] })).toBeNull()

    const catalog = parseProviderCatalog({
      version: 1,
      revision: 7,
      providers: [
        {
          id: 'p',
          name: 'Provider',
          type: 'openai',
          custom_model_id: true,
          key_selection_required: true,
          keys: [{ id: 'k', label: 'Key', availability: 'available' }],
          models: [{ id: 'm', name: 'Model', text_generation: false, context_limit: 100 }],
        },
      ],
    })
    expect(catalog.revision).toBe(7)
    expect(catalog.providers[0]).toMatchObject({
      id: 'p',
      custom_model_id: true,
      key_selection_required: true,
    })
    expect(catalog.providers[0]?.models[0]).toMatchObject({
      id: 'm',
      text_generation: false,
      context_limit: 100,
      output_limit: null,
    })
  })

  it('parses session views and events', () => {
    expect(parseSessionView({ session_id: 'c', revision: 3, event_seq: 8, unchanged: true })).toEqual({
      session_id: 'c',
      revision: 3,
      event_seq: 8,
      conversation: undefined,
      unchanged: true,
    })
    expect(parseSessionView({ revision: 3 })).toBeNull()
    expect(
      parseEvent({ session_id: 'c', seq: 4, kind: 'delta', request_id: 'r', data: { text: 'hi' } }),
    ).toMatchObject({ session_id: 'c', seq: 4, kind: 'delta', data: { text: 'hi' } })
    expect(parseEvent({ session_id: 'c', seq: '4', kind: 'delta' })).toBeNull()
  })
})
