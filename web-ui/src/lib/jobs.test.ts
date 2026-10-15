import { describe, expect, it } from 'vitest'
import type { GryviaAIJob, GryviaGpuNode, GryviaQuota } from '@/types'
import {
  EMPTY_FORM, argvPreview, capacityHints, envSource, formIsDirty, gpuShareAnnotations, gpuShareError, gpuTypeOptions, isSensitiveEnv, jobConditions, jobSearchText,
  jobStatusGroup, jobToForm, joinCommand, logText, nameTaken, podNodes, priorityError, retryLimitError, splitCommand, timeoutError,
} from './jobs'

describe('env redaction', () => {
  it('masks credential-like names only', () => {
    for (const n of ['PASSWORD', 'DB_PASSWORD', 'HF_TOKEN', 'API_KEY', 'APIKEY', 'aws_secret', 'PRIVATE_KEY', 'CREDENTIALS']) expect(isSensitiveEnv(n)).toBe(true)
    for (const n of ['KEYBOARD', 'MONKEY', 'AUTHOR', 'DATABASE_URL', 'CONNECTIONS', 'BATCH_SIZE', 'TOKENIZERS_PARALLELISM']) expect(isSensitiveEnv(n)).toBe(false)
  })
  it('describes references', () => {
    expect(envSource({ name: 'A', value: 'x' })).toBeUndefined()
    expect(envSource({ name: 'A', valueFrom: { secretKeyRef: { name: 'db', key: 'pw' } } })).toBe('from secret/db (pw)')
    expect(envSource({ name: 'A', valueFrom: { configMapKeyRef: { name: 'cfg' } } })).toBe('from configmap/cfg')
  })
})

const job = (over: Partial<GryviaAIJob['spec']> & Record<string, unknown> = {}, labels: Record<string, string> = {}): GryviaAIJob =>
  ({
    apiVersion: 'gryvia.io/v1alpha1',
    kind: 'GryviaAIJob',
    metadata: { name: 'src', labels },
    spec: { type: 'fine-tuning', image: 'img:1', gpus: 8, gpuType: 'A100-80G', command: ['python', 'train.py'], args: ['--name', 'a b'], ...over },
  }) as GryviaAIJob

const node = (gpuType: string) => ({ metadata: { name: gpuType }, spec: { gpuType, gpuCount: 8, nodeName: gpuType } }) as GryviaGpuNode
const quota = (allowed: string[], maxGPUs = 16, perJob = 8, allocated = 0): GryviaQuota =>
  ({ metadata: { name: 'q' }, spec: { team: 'ml', gpuQuota: { maxGPUs, maxGPUsPerJob: perJob, allowedGPUTypes: allowed, maxRunningJobs: 5 } }, status: { currentUsage: { allocatedGPUs: allocated } } }) as GryviaQuota

describe('command splitting', () => {
  it('round-trips quoted arguments', () => {
    expect(splitCommand(`python train.py --name "a b" --x='c d'`)).toEqual(['python', 'train.py', '--name', 'a b', '--x=c d'])
    expect(splitCommand(`echo ""`)).toEqual(['echo', ''])
    expect(splitCommand(joinCommand(['run', 'a b', "it's", '']))).toEqual(['run', 'a b', "it's", ''])
    expect(argvPreview('a "b c"')).toBe('[a] [b c]')
    expect(argvPreview('   ')).toBe('')
  })
})

describe('jobToForm', () => {
  it('maps a job onto the form, blank name, team and secrets', () => {
    const { values, skippedEnv } = jobToForm(
      job(
        {
          priority: 40,
          timeout: '2h',
          retryLimit: 3,
          resources: { requests: { cpu: '16', memory: '64Gi' } },
          distributed: { enabled: true, framework: 'jax', nodes: 2, gpusPerNode: 4 },
          env: [{ name: 'BATCH', value: '8' }, { name: 'HF_TOKEN', value: 'x' }, { name: 'FROM_SECRET', valueFrom: { secretKeyRef: { name: 's' } } }] as never,
        },
        { 'gryvia.io/team': 'ml' },
      ),
    )
    expect(skippedEnv).toBe(1)
    expect(values).toMatchObject({ name: '', type: 'fine-tuning', team: 'ml', framework: 'jax', gpuType: 'A100-80G', gpuCount: 8, memory: '64Gi', cpu: 16, distributedEnabled: true, nodes: 2, gpusPerNode: 4, priority: '40', timeout: '2h', retryLimit: '3' })
    expect(values.command).toBe("python train.py --name 'a b'")
    expect(values.env.map((e) => [e.name, e.secret])).toEqual([['BATCH', false], ['HF_TOKEN', true]])
  })
  it('falls back to defaults for a sparse job', () => {
    const { values } = jobToForm({ metadata: { name: 'x' }, spec: {} } as unknown as GryviaAIJob)
    expect(values).toMatchObject({ type: 'training', framework: 'pytorch', gpuCount: 1, priority: '', timeout: '' })
  })
  it('detects dirtiness', () => {
    expect(formIsDirty(EMPTY_FORM, EMPTY_FORM)).toBe(false)
    expect(formIsDirty({ ...EMPTY_FORM, name: 'a' }, EMPTY_FORM)).toBe(true)
  })
  it('clones a shared-GPU request and ignores unknown shares', () => {
    const shared = jobToForm({
      metadata: { name: 'x', annotations: { 'gryvia.io/gpu-share': '0.5', 'gryvia.io/gpu-memory': '20480' } }, spec: { gpus: 1 },
    } as unknown as GryviaAIJob).values
    expect(shared).toMatchObject({ gpuShare: '0.5', gpuMemoryMiB: '20480', gpuCount: 1 })
    const odd = jobToForm({ metadata: { name: 'x', annotations: { 'gryvia.io/gpu-share': '0.33' } }, spec: {} } as unknown as GryviaAIJob).values
    expect(odd).toMatchObject({ gpuShare: '', gpuMemoryMiB: '' })
  })
})

describe('gpu share', () => {
  it('builds annotations only for a fraction', () => {
    expect(gpuShareAnnotations({ gpuShare: '', gpuMemoryMiB: '1024' })).toEqual({})
    expect(gpuShareAnnotations({ gpuShare: '0.25', gpuMemoryMiB: '' })).toEqual({ 'gryvia.io/gpu-share': '0.25' })
    expect(gpuShareAnnotations({ gpuShare: '0.5', gpuMemoryMiB: ' 20480 ' })).toEqual({
      'gryvia.io/gpu-share': '0.5', 'gryvia.io/gpu-memory': '20480',
    })
  })
  it('validates', () => {
    expect(gpuShareError({ gpuShare: '', gpuMemoryMiB: 'x', distributedEnabled: true })).toBeUndefined()
    expect(gpuShareError({ gpuShare: '0.5', gpuMemoryMiB: '', distributedEnabled: true })).toMatch(/single-pod/)
    expect(gpuShareError({ gpuShare: '0.5', gpuMemoryMiB: '20G', distributedEnabled: false })).toMatch(/MiB/)
    expect(gpuShareError({ gpuShare: '0.5', gpuMemoryMiB: '20480', distributedEnabled: false })).toBeUndefined()
  })
})

describe('gpuTypeOptions', () => {
  it('uses distinct node types, sorted, limited by the quota', () => {
    const nodes = [node('H100'), node('A100-80G'), node('H100')]
    expect(gpuTypeOptions(nodes)).toEqual(['A100-80G', 'H100'])
    expect(gpuTypeOptions(nodes, quota(['H100', 'T4']))).toEqual(['H100'])
    expect(gpuTypeOptions(nodes, quota([]))).toEqual(['A100-80G', 'H100'])
    expect(gpuTypeOptions(nodes, quota(['T4']))).toEqual(['T4'])
  })
  it('falls back to the standard list and keeps the current value', () => {
    expect(gpuTypeOptions([])).toContain('H100')
    expect(gpuTypeOptions([node('H100')], undefined, 'L4')).toEqual(['H100', 'L4'])
  })
})

describe('advanced validation', () => {
  it('validates timeout', () => {
    for (const v of ['', '2h', '90m', '7d', '99999s']) expect(timeoutError(v)).toBeUndefined()
    for (const v of ['0h', '2', 'h', '2 h', '100000s', '2w', '-1h']) expect(timeoutError(v)).toBeTruthy()
  })
  it('validates priority and retry limit', () => {
    expect(priorityError('')).toBeUndefined()
    expect(priorityError('0')).toBeUndefined()
    expect(priorityError('100')).toBeUndefined()
    expect(priorityError('101')).toBeTruthy()
    expect(priorityError('-1')).toBeTruthy()
    expect(priorityError('1.5')).toBeTruthy()
    expect(retryLimitError('10')).toBeUndefined()
    expect(retryLimitError('11')).toBeTruthy()
  })
})

describe('capacityHints', () => {
  const cluster = { totalGPUs: 16, availableGPUs: 4 }
  it('warns about cluster and quota limits without blocking', () => {
    expect(capacityHints(2, cluster)).toEqual([])
    expect(capacityHints(8, cluster)[0]).toMatch(/Only 4 of 16/)
    expect(capacityHints(32, cluster)[0]).toMatch(/cannot be scheduled/)
    const h = capacityHints(9, cluster, quota(['H100'], 16, 8, 10), 'T4')
    expect(h.some((m) => /at most 8 GPUs per job/.test(m))).toBe(true)
    expect(h.some((m) => /exceed the team quota/.test(m))).toBe(true)
    expect(h.some((m) => /not allowed to use T4/.test(m))).toBe(true)
  })
  it('stays quiet without data', () => {
    expect(capacityHints(4)).toEqual([])
  })
})

describe('list helpers', () => {
  it('searches name, image, team and framework and groups status', () => {
    const j = job({ image: 'nvcr.io/pt:1' }, { 'gryvia.io/team': 'ml', 'gryvia.io/framework': 'jax' })
    expect(jobSearchText(j)).toContain('nvcr.io/pt:1')
    expect(jobSearchText(j)).toContain('ml')
    expect(jobStatusGroup({ ...j, status: { phase: 'Succeeded' } })).toBe('Completed')
    expect(jobStatusGroup(j)).toBe('Other')
  })
  it('detects duplicate names and orders conditions', () => {
    expect(nameTaken('src', [job()])).toBe(true)
    expect(nameTaken('', [job()])).toBe(false)
    const withConds = { ...job(), status: { phase: 'Running', conditions: [{ type: 'B', status: 'True', lastTransitionTime: '2026-01-02T00:00:00Z' }, { type: 'A', status: 'True', lastTransitionTime: '2026-01-01T00:00:00Z' }, { type: 'C', status: 'True' }] } } as unknown as GryviaAIJob
    expect(jobConditions(withConds).map((c) => c.type)).toEqual(['A', 'B', 'C'])
    expect(podNodes([{ node: 'n1' }, { node: 'n1' }, {}, { node: 'n2' }])).toEqual(['n1', 'n2'])
    expect(logText([])).toBe('')
    expect(logText(['a', 'b'])).toBe('a\nb\n')
  })
})
