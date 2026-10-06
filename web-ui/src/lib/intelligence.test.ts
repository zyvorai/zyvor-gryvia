import { describe, expect, it } from 'vitest'
import { initial, resolved, type Schema } from './intelligence'

describe('analysis forms', () => {
  it('resolves nested definitions and includes required fields without inventing optional telemetry', () => {
    const schema: Schema = {
      type: 'object',
      required: ['ranks'],
      properties: {
        ranks: { type: 'array', items: { $ref: '#/$defs/Rank' } },
        optional: { type: 'number' },
      },
      $defs: {
        Rank: {
          type: 'object',
          required: ['rank', 'observedAt'],
          properties: {
            rank: { type: 'integer', minimum: 0 },
            observedAt: { type: 'string', format: 'date-time' },
          },
        },
      },
    }
    expect(initial(schema, schema)).toEqual({ ranks: [] })
    expect(initial(schema.properties!.ranks.items!, schema)).toEqual({
      rank: 0,
      observedAt: '',
    })
  })
  it('preserves false and zero defaults and handles nullable fields', () => {
    expect(initial({ type: 'boolean', default: false }, {})).toBe(false)
    expect(initial({ type: 'integer', default: 0 }, {})).toBe(0)
    expect(
      resolved(
        { anyOf: [{ type: 'null' }, { type: 'number', minimum: 1 }] },
        {},
      ),
    ).toEqual({ type: 'number', minimum: 1 })
  })
})
