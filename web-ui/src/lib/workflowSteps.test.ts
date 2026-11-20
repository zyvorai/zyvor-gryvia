// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import {
  DEFAULT_STEPS,
  emptyStep,
  initialSpecState,
  jsonToSpec,
  joinCommand,
  specBody,
  specError,
  specToGateway,
  splitCommand,
  validateSpec,
  withSpec,
  withSpecJson,
} from './workflowSteps'

describe('command lines', () => {
  it('splits with quotes and joins back', () => {
    expect(splitCommand(`python train.py --name 'a b' "c d"`)).toEqual(['python', 'train.py', '--name', 'a b', 'c d'])
    expect(splitCommand('')).toEqual([])
    expect(splitCommand("echo 'oops")).toBeNull()
    expect(splitCommand(`echo ''`)).toEqual(['echo', ''])
    expect(splitCommand(joinCommand(['sh', '-c', 'echo hi there', "it's", '']))).toEqual(['sh', '-c', 'echo hi there', "it's", ''])
  })
})

describe('steps <-> gateway JSON', () => {
  it('converts the default steps to the gateway shape', () => {
    expect(specToGateway(DEFAULT_STEPS(), [])).toEqual({
      steps: [
        { name: 'prepare', type: 'job', jobTemplate: { type: 'training', image: 'busybox:1.36', gpus: 0, command: ['echo', 'prepare'] } },
        { name: 'train', type: 'job', dependsOn: ['prepare'], jobTemplate: { type: 'training', image: 'busybox:1.36', gpus: 1, command: ['echo', 'train'] } },
      ],
    })
  })
  it('covers script and webhook steps, retries, timeout and parameters', () => {
    const steps = [
      emptyStep({ name: 's', type: 'script', image: 'python:3', command: 'python -c "print(1)"', retries: '2', timeoutSeconds: '60' }),
      emptyStep({ name: 'w', type: 'webhook', url: 'https://x.io/hook', method: 'PUT', body: '{"a":1}', dependsOn: ['s'] }),
    ]
    const body = specToGateway(steps, [{ id: 'x', key: 'env', value: 'prod' }, { id: 'y', key: '', value: '' }])
    expect(body).toEqual({
      steps: [
        { name: 's', type: 'script', script: { image: 'python:3', command: ['python', '-c', 'print(1)'] }, retries: 2, timeoutSeconds: 60 },
        { name: 'w', type: 'webhook', dependsOn: ['s'], webhook: { url: 'https://x.io/hook', method: 'PUT', body: '{"a":1}' } },
      ],
      parameters: { env: 'prod' },
    })
  })
  it('round-trips through JSON', () => {
    const steps = [
      emptyStep({ name: 'a', image: 'img', gpus: '2', gpuType: 'A100', command: 'run --x "y z"', retries: '1' }),
      emptyStep({ name: 'b', type: 'webhook', url: 'http://h/x', dependsOn: ['a'] }),
    ]
    const json = JSON.stringify(specToGateway(steps, [{ id: 'p', key: 'k', value: 'v' }]))
    const parsed = jsonToSpec(json)
    if ('error' in parsed) throw new Error(parsed.error)
    expect(JSON.stringify(specToGateway(parsed.steps, parsed.params))).toBe(json)
  })
  it('accepts a bare array and rejects what the editor cannot show', () => {
    expect('steps' in jsonToSpec('[{"name":"a","type":"job","jobTemplate":{"image":"i","gpus":0}}]')).toBe(true)
    expect(jsonToSpec('{')).toHaveProperty('error')
    expect(jsonToSpec('{"steps":[]}')).toHaveProperty('error')
    expect(jsonToSpec('{"steps":[{"name":"a","type":"job","jobTemplate":{"image":"i","gpus":0,"args":["x"]}}]}')).toHaveProperty('error')
    expect(jsonToSpec('{"steps":[{"name":"a","type":"job"}]}')).toHaveProperty('error')
  })
})

describe('validateSpec', () => {
  it('accepts the defaults', () => {
    expect(validateSpec(DEFAULT_STEPS(), []).valid).toBe(true)
  })
  it('flags field problems per step', () => {
    const v = validateSpec(
      [
        emptyStep({ name: 'Bad_Name', image: '' }),
        emptyStep({ name: 'sc', type: 'script', image: 'i', command: '' }),
        emptyStep({ name: 'wh', type: 'webhook', url: 'ftp://x', retries: '11' }),
      ],
      [],
    )
    expect(v.steps[0].name).toBeDefined()
    expect(v.steps[0].image).toBeDefined()
    expect(v.steps[1].command).toMatch(/command/)
    expect(v.steps[2].url).toBeDefined()
    expect(v.steps[2].retries).toBeDefined()
    expect(v.valid).toBe(false)
  })
  it('flags duplicate names, unknown dependencies and cycles inline', () => {
    const base = { image: 'i' }
    const dup = validateSpec([emptyStep({ name: 'a', ...base }), emptyStep({ name: 'a', ...base })], [])
    expect(dup.steps[0].graph).toMatch(/more than once/)
    const unknown = validateSpec([emptyStep({ name: 'a', dependsOn: ['zzz'], ...base })], [])
    expect(unknown.steps[0].graph).toMatch(/unknown step "zzz"/)
    const cycle = validateSpec([emptyStep({ name: 'a', dependsOn: ['b'], ...base }), emptyStep({ name: 'b', dependsOn: ['a'], ...base })], [])
    expect(cycle.steps.every((e) => /cycle/.test(e.graph ?? ''))).toBe(true)
    expect(cycle.list[0]).toMatch(/cycle/)
  })
  it('checks parameter keys', () => {
    const v = validateSpec(DEFAULT_STEPS(), [
      { id: '1', key: 'a', value: '1' },
      { id: '2', key: 'a', value: '2' },
      { id: '3', key: '', value: 'orphan' },
    ])
    expect(v.params).toEqual(['Keys must be unique.', 'Keys must be unique.', 'Enter a key.'])
    expect(v.valid).toBe(false)
  })
})

describe('editor state', () => {
  it('keeps JSON in sync in both directions', () => {
    const s = initialSpecState()
    const edited = withSpec(s, [emptyStep({ name: 'only', image: 'i' })], [])
    expect(JSON.parse(edited.json).steps[0].name).toBe('only')
    const back = withSpecJson(edited, '{"steps":[{"name":"x","type":"webhook","webhook":{"url":"https://h"}}],"parameters":{"a":"b"}}')
    expect(back.steps[0]).toMatchObject({ name: 'x', type: 'webhook', url: 'https://h' })
    expect(back.params[0]).toMatchObject({ key: 'a', value: 'b' })
    const broken = withSpecJson(back, '{"steps":')
    expect(broken.steps).toBe(back.steps)
  })
  it('validates the JSON in advanced mode, including JSON the editor cannot show', () => {
    const adv = { ...initialSpecState(), advanced: true }
    expect(specError(adv)).toBeNull()
    expect(specError({ ...adv, json: '[' })).toMatch(/JSON/)
    const cyc = '{"steps":[{"name":"a","dependsOn":["b"]},{"name":"b","dependsOn":["a"]}]}'
    expect(specError({ ...adv, json: cyc })).toMatch(/cycle/)
    const extra = '{"steps":[{"name":"a","type":"job","jobTemplate":{"image":"i","gpus":0,"args":["x"]}}]}'
    expect(specError({ ...adv, json: extra })).toBeNull()
    expect(specBody({ ...adv, json: extra }).steps).toHaveLength(1)
  })
})
