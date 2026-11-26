import { describe, expect, it } from 'vitest'
import { base64ByteLength, decodeBase64Chunks, encodeBase64Utf8, formatBytes, utf8Bytes } from './bytes'

describe('byte helpers', () => {
  it('counts UTF-8 bytes, not UTF-16 code units', () => {
    expect(utf8Bytes('abc')).toBe(3)
    expect(utf8Bytes('ắ')).toBe(3)
    expect(utf8Bytes('😀')).toBe(4)
    expect(utf8Bytes('a😀ắ')).toBe(8)
  })

  it('round-trips independently encoded chunks without corrupting multibyte text', () => {
    const text = 'Xin chào 😀 — Generative'
    const chunks = [encodeBase64Utf8('Xin chào '), encodeBase64Utf8('😀 — Generative')]
    expect(decodeBase64Chunks(chunks)).toBe(text)
    expect(chunks.reduce((total, chunk) => total + base64ByteLength(chunk), 0)).toBe(utf8Bytes(text))
  })

  it('formats human-readable byte counts', () => {
    expect(formatBytes(12)).toBe('12 B')
    expect(formatBytes(2048)).toBe('2.0 KiB')
    expect(formatBytes(3 << 20)).toBe('3.0 MiB')
  })
})
