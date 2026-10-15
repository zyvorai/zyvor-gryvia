import type { GryviaAIJob, GryviaGpuNode, GryviaQuota } from '@/types'
import { phaseGroup } from './phase'

export const FRAMEWORK_LABEL = 'gryvia.io/framework'

/** ML framework of a job: distributed config, then the label the dashboard sets, then the job type. */
export function jobFramework(job: GryviaAIJob): string {
  return job.spec?.distributed?.framework || job.metadata?.labels?.[FRAMEWORK_LABEL] || job.spec?.type || 'job'
}

/** "8 × H100", or "8 × GPU" when the type is unset. */
export function jobGpus(job: GryviaAIJob): string {
  return `${job.spec?.gpus ?? 0} × ${job.spec?.gpuType || 'GPU'}`
}

const SENSITIVE_ENV = /(^|_)(PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY|CREDENTIALS?)($|_)/i

/** True when an env var's value should be masked until the user reveals it. */
export function isSensitiveEnv(name: string): boolean {
  return SENSITIVE_ENV.test(name)
}

export type EnvEntry = {
  name: string
  value?: string
  valueFrom?: {
    secretKeyRef?: { name?: string; key?: string }
    configMapKeyRef?: { name?: string; key?: string }
  }
}

/** Where an env var's value comes from when it is a reference, e.g. "from secret/db-creds"; undefined for literals. */
export function envSource(env: EnvEntry): string | undefined {
  const s = env.valueFrom?.secretKeyRef
  if (s) return `from secret/${s.name ?? 'unknown'}${s.key ? ` (${s.key})` : ''}`
  const c = env.valueFrom?.configMapKeyRef
  if (c) return `from configmap/${c.name ?? 'unknown'}${c.key ? ` (${c.key})` : ''}`
  if (env.valueFrom) return 'from reference'
  return undefined
}

// ---------------------------------------------------------------------------------------------
// Fields the CRD has but the shared GryviaAIJob type does not declare yet (see gryvia.io_gryviaaijobs).
export type JobCondition = { type: string; status: string; reason?: string; message?: string; lastTransitionTime?: string }
export type JobSpecExtras = { priority?: number; timeout?: string; retryLimit?: number }
type JobExtras = GryviaAIJob & { spec: GryviaAIJob['spec'] & JobSpecExtras; status?: { conditions?: JobCondition[] } }

export const TEAM_LABEL = 'gryvia.io/team'
export const JOB_TYPES = ['training', 'inference', 'fine-tuning', 'evaluation'] as const
export const NO_TEAM = ''
export const FALLBACK_GPU_TYPES = ['H100', 'A100-80G', 'A100-40G', 'L40', 'V100', 'T4']
/** The gateway caps a list at this many items; a full page means older jobs may be missing. */
export const JOB_LIST_LIMIT = 500

/** Team label of a job, if any. */
export function jobTeam(job: GryviaAIJob): string | undefined {
  return job.metadata?.labels?.[TEAM_LABEL] || undefined
}

/** Text the jobs search box matches: name, image, team and framework. */
export function jobSearchText(job: GryviaAIJob): string {
  return [job.metadata?.name, job.spec?.image, jobTeam(job), jobFramework(job)].filter(Boolean).join(' ')
}

/** Status filter value: the coarse group, so Succeeded and Completed count as one. */
export function jobStatusGroup(job: GryviaAIJob): string {
  const g = phaseGroup(job.status?.phase)
  return g.charAt(0).toUpperCase() + g.slice(1)
}
export const STATUS_FILTER_OPTIONS = ['Pending', 'Running', 'Completed', 'Failed', 'Other']

export function jobCreatedMs(job: GryviaAIJob): number | undefined {
  const t = job.metadata?.creationTimestamp ? new Date(job.metadata.creationTimestamp).getTime() : NaN
  return Number.isNaN(t) ? undefined : t
}

/** Conditions in the order they happened (oldest first); entries without a time keep their place at the end. */
export function jobConditions(job: GryviaAIJob): JobCondition[] {
  const list = ((job as JobExtras).status?.conditions ?? []).filter((c) => c && c.type)
  const time = (c: JobCondition) => (c.lastTransitionTime ? new Date(c.lastTransitionTime).getTime() : Infinity)
  return list.map((c, i) => ({ c, i })).sort((a, b) => time(a.c) - time(b.c) || a.i - b.i).map((x) => x.c)
}

/** Distinct node names a job's pods ran on. */
export function podNodes(pods: Array<{ node?: string }>): string[] {
  return [...new Set(pods.map((p) => p.node).filter((n): n is string => Boolean(n)))]
}

// ---------------------------------------------------------------------------------------------
// Submit form: values, cloning, validation.

export type EnvRow = { id: number; name: string; value: string; secret: boolean }
export type JobFormValues = {
  name: string
  type: string
  team: string
  framework: string
  gpuType: string
  gpuCount: number
  /** '' for whole GPUs, else a fraction of one GPU ('0.25', '0.5', '0.75'). */
  gpuShare: string
  /** Optional GPU memory limit in MiB for a shared GPU. */
  gpuMemoryMiB: string
  memory: string
  cpu: number
  image: string
  command: string
  distributedEnabled: boolean
  nodes: number
  gpusPerNode: number
  priority: string
  timeout: string
  retryLimit: string
  env: EnvRow[]
}

export const EMPTY_FORM: JobFormValues = {
  name: '',
  type: 'training',
  team: NO_TEAM,
  framework: 'pytorch',
  gpuType: 'H100',
  gpuCount: 1,
  gpuShare: '',
  gpuMemoryMiB: '',
  memory: '32Gi',
  cpu: 8,
  image: 'nvcr.io/nvidia/pytorch:24.01-py3',
  command: '',
  distributedEnabled: false,
  nodes: 1,
  gpusPerNode: 1,
  priority: '',
  timeout: '',
  retryLimit: '',
  env: [],
}

/** Shell-like split honouring single and double quotes (no escapes). */
export function splitCommand(cmd: string): string[] {
  const args: string[] = []
  let current = ''
  let quote = ''
  let started = false
  for (const char of cmd) {
    if (quote) {
      if (char === quote) quote = ''
      else current += char
    } else if (char === '"' || char === "'") {
      quote = char
      started = true
    } else if (/\s/.test(char)) {
      if (current || started) {
        args.push(current)
        current = ''
        started = false
      }
    } else {
      current += char
    }
  }
  if (current || started) args.push(current)
  return args
}

/** Inverse of splitCommand for display and cloning: quotes only what needs it. */
export function joinCommand(argv: string[]): string {
  return argv
    .map((a) => {
      if (a !== '' && !/[\s"']/.test(a)) return a
      return a.includes("'") ? `"${a}"` : `'${a}'`
    })
    .join(' ')
}

/** Argv preview line, one bracketed token per argument so quoting mistakes are visible. */
export function argvPreview(cmd: string): string {
  const argv = splitCommand(cmd)
  return argv.length === 0 ? '' : argv.map((a) => `[${a}]`).join(' ')
}

/** Map an existing job onto the form. `skippedEnv` counts variables that reference a Secret/ConfigMap (not copied). */
export function jobToForm(job: GryviaAIJob): { values: JobFormValues; skippedEnv: number } {
  const spec = job.spec as JobExtras['spec'] | undefined
  const dist = spec?.distributed
  const env = (spec?.env ?? []) as EnvEntry[]
  const literals = env.filter((e) => !e.valueFrom)
  const framework = dist?.framework || job.metadata?.labels?.[FRAMEWORK_LABEL] || EMPTY_FORM.framework
  const argv = [...(spec?.command ?? []), ...(spec?.args ?? [])]
  return {
    skippedEnv: env.length - literals.length,
    values: {
      ...EMPTY_FORM,
      // A clone needs its own name; the user picks it.
      name: '',
      type: spec?.type || EMPTY_FORM.type,
      team: job.metadata?.labels?.[TEAM_LABEL] ?? NO_TEAM,
      framework,
      gpuType: spec?.gpuType || EMPTY_FORM.gpuType,
      gpuCount: spec?.gpus && spec.gpus > 0 ? spec.gpus : 1,
      ...shareFromAnnotations(job.metadata?.annotations),
      memory: spec?.resources?.requests?.memory ?? EMPTY_FORM.memory,
      cpu: Number.parseInt(String(spec?.resources?.requests?.cpu ?? ''), 10) || EMPTY_FORM.cpu,
      image: spec?.image || EMPTY_FORM.image,
      command: joinCommand(argv),
      distributedEnabled: Boolean(dist?.enabled),
      nodes: dist?.nodes || 1,
      gpusPerNode: dist?.gpusPerNode || 1,
      priority: spec?.priority === undefined ? '' : String(spec.priority),
      timeout: spec?.timeout ?? '',
      retryLimit: spec?.retryLimit === undefined ? '' : String(spec.retryLimit),
      env: literals.map((e, i) => ({ id: i + 1, name: e.name, value: e.value ?? '', secret: isSensitiveEnv(e.name) })),
    },
  }
}

export const GPU_SHARE_ANNOTATION = 'gryvia.io/gpu-share'
export const GPU_MEMORY_ANNOTATION = 'gryvia.io/gpu-memory'
export const GPU_SHARES: [string, string][] = [
  ['', 'Whole GPUs'],
  ['0.25', '1/4 of a GPU'],
  ['0.5', '1/2 of a GPU'],
  ['0.75', '3/4 of a GPU'],
]

/** Annotations that request part of one GPU (the native queue scheduler enforces them), or none. */
export function gpuShareAnnotations(values: Pick<JobFormValues, 'gpuShare' | 'gpuMemoryMiB'>): Record<string, string> {
  if (!values.gpuShare) return {}
  const out: Record<string, string> = { [GPU_SHARE_ANNOTATION]: values.gpuShare }
  if (values.gpuMemoryMiB.trim() !== '') out[GPU_MEMORY_ANNOTATION] = values.gpuMemoryMiB.trim()
  return out
}

/** Inline error for the shared-GPU fields, or undefined. */
export function gpuShareError(values: Pick<JobFormValues, 'gpuShare' | 'gpuMemoryMiB' | 'distributedEnabled'>): string | undefined {
  if (!values.gpuShare) return undefined
  if (values.distributedEnabled) return 'A shared GPU is for single-pod jobs; turn off distributed training or use whole GPUs.'
  const mem = values.gpuMemoryMiB.trim()
  if (mem !== '' && !/^[1-9][0-9]{0,6}$/.test(mem)) return 'GPU memory is a whole number of MiB, for example 20480.'
  return undefined
}

function shareFromAnnotations(ann: Record<string, string> | undefined): Pick<JobFormValues, 'gpuShare' | 'gpuMemoryMiB'> {
  const share = ann?.[GPU_SHARE_ANNOTATION]
  if (!share || !GPU_SHARES.some(([v]) => v === share)) return { gpuShare: '', gpuMemoryMiB: '' }
  return { gpuShare: share, gpuMemoryMiB: ann?.[GPU_MEMORY_ANNOTATION] ?? '' }
}

/** True when the form differs from what it started as (ignoring env row ids). */
export function formIsDirty(current: JobFormValues, initial: JobFormValues): boolean {
  const strip = (f: JobFormValues) => JSON.stringify({ ...f, env: f.env.map((e) => [e.name, e.value, e.secret]) })
  return strip(current) !== strip(initial)
}

/** GPU type choices: types present on nodes, limited to the team's allowed types when it sets any. */
export function gpuTypeOptions(nodes: GryviaGpuNode[] | undefined, quota?: GryviaQuota, current?: string): string[] {
  const fromNodes = [...new Set((nodes ?? []).map((n) => n.spec?.gpuType).filter((t): t is string => Boolean(t)))].sort((a, b) => a.localeCompare(b))
  const allowed = quota?.spec?.gpuQuota?.allowedGPUTypes
  let opts = fromNodes.length > 0 ? fromNodes : FALLBACK_GPU_TYPES
  if (allowed && allowed.length > 0) {
    const inter = opts.filter((t) => allowed.includes(t))
    opts = inter.length > 0 ? inter : [...allowed]
  }
  return current && !opts.includes(current) && !(allowed && allowed.length > 0) ? [...opts, current] : opts
}

const TIMEOUT = /^[1-9][0-9]{0,4}[smhd]$/

/** Empty is fine (no timeout). */
export function timeoutError(v: string): string | undefined {
  if (v.trim() === '') return undefined
  return TIMEOUT.test(v.trim()) ? undefined : 'Use a number and a unit: s, m, h or d (for example 2h or 7d).'
}

function boundedInt(v: string, min: number, max: number, what: string): string | undefined {
  if (v.trim() === '') return undefined
  if (!/^\d+$/.test(v.trim())) return `${what} must be a whole number from ${min} to ${max}.`
  const n = Number(v)
  return n < min || n > max ? `${what} must be from ${min} to ${max}.` : undefined
}
export const priorityError = (v: string) => boundedInt(v, 0, 100, 'Priority')
export const retryLimitError = (v: string) => boundedInt(v, 0, 10, 'Retry limit')

export type ClusterCapacity = { totalGPUs: number; availableGPUs: number }

/** Non-blocking warnings about a GPU request; the server stays authoritative. */
export function capacityHints(gpus: number, cluster?: ClusterCapacity, quota?: GryviaQuota, gpuType?: string): string[] {
  const out: string[] = []
  if (cluster && cluster.totalGPUs > 0) {
    if (gpus > cluster.totalGPUs) out.push(`Requests ${gpus} GPUs but the cluster has ${cluster.totalGPUs} in total; the job cannot be scheduled.`)
    else if (gpus > cluster.availableGPUs) out.push(`Only ${cluster.availableGPUs} of ${cluster.totalGPUs} GPUs are free now; the job will wait in the queue.`)
  }
  const q = quota?.spec?.gpuQuota
  if (q) {
    const team = quota!.spec.team
    if (q.maxGPUsPerJob > 0 && gpus > q.maxGPUsPerJob) out.push(`Team ${team} allows at most ${q.maxGPUsPerJob} GPUs per job.`)
    const allocated = quota!.status?.currentUsage?.allocatedGPUs ?? 0
    if (q.maxGPUs > 0 && allocated + gpus > q.maxGPUs) out.push(`Team ${team} has ${allocated} of ${q.maxGPUs} GPUs allocated; this job would exceed the team quota.`)
    if (gpuType && q.allowedGPUTypes?.length > 0 && !q.allowedGPUTypes.includes(gpuType)) out.push(`Team ${team} is not allowed to use ${gpuType}.`)
  }
  return out
}

/** True when a job with this name already exists (Kubernetes names are case-sensitive). */
export function nameTaken(name: string, jobs: GryviaAIJob[] | undefined): boolean {
  return Boolean(name) && (jobs ?? []).some((j) => j.metadata?.name === name)
}

/** Log lines as one text blob, for copy and download. */
export function logText(lines: string[]): string {
  return lines.length === 0 ? '' : lines.join('\n') + '\n'
}
