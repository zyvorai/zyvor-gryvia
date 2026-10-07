import { useEffect, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  EMPTY_FORM, FRAMEWORK_LABEL, JOB_TYPES, NO_TEAM, TEAM_LABEL, argvPreview, capacityHints, formIsDirty, gpuTypeOptions, jobToForm,
  nameTaken, priorityError, retryLimitError, splitCommand, timeoutError, type JobFormValues,
} from '@/lib/jobs'
import { api } from '@/lib/api'
import { errorMessage } from '@/lib/errors'
import type { GryviaAIJob } from '@/types'
import { Trash2 } from 'lucide-react'
import PageHero from '@/components/PageHero'
import ConfirmDialog from '@/components/ConfirmDialog'
import { ErrorState, Skeleton } from '@/components/StateViews'
import { notify } from '@/lib/notify'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'

const K8S_NAME = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/
const NAME_HELP = 'Lowercase letters, digits and hyphens; start and end with a letter or digit; at most 63 characters.'
const ENV_NAME = /^[A-Za-z_][A-Za-z0-9_]*$/

/** Inline error for a job name, or undefined when valid (empty is reported only after the field is touched). */
function nameError(name: string): string | undefined {
  if (name.length === 0) return 'Job name is required.'
  if (name.length > 63) return `Job name is ${name.length} characters; the maximum is 63.`
  if (!K8S_NAME.test(name)) return NAME_HELP
  return undefined
}

export default function SubmitJob() {
  useDocumentTitle('Submit job')
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [searchParams] = useSearchParams()
  const cloneName = searchParams.get('clone') || undefined
  const [nameTouched, setNameTouched] = useState(false)
  const [formData, setFormData] = useState<JobFormValues>(EMPTY_FORM)
  const [initial, setInitial] = useState<JobFormValues>(EMPTY_FORM)
  const [appliedClone, setAppliedClone] = useState<string | undefined>()
  const [skippedEnv, setSkippedEnv] = useState(0)
  const [shown, setShown] = useState<Set<number>>(new Set())
  const [confirmLeave, setConfirmLeave] = useState(false)

  const cloneQ = useQuery({ queryKey: ['job', cloneName], queryFn: () => api.getJob(cloneName!), enabled: !!cloneName, retry: 1 })
  const jobsQ = useQuery({ queryKey: ['jobs'], queryFn: api.getJobs })
  const nodesQ = useQuery({ queryKey: ['nodes'], queryFn: api.getNodes })
  const quotasQ = useQuery({ queryKey: ['quotas'], queryFn: api.getQuotas })
  const statsQ = useQuery({ queryKey: ['clusterStats'], queryFn: api.getClusterStats })

  // Apply a clone once it has loaded (adjusting state during render, not in an effect).
  if (cloneQ.data && appliedClone !== cloneQ.data.metadata.name) {
    const { values, skippedEnv: skipped } = jobToForm(cloneQ.data)
    setAppliedClone(cloneQ.data.metadata.name)
    setFormData(values)
    setInitial(values)
    setSkippedEnv(skipped)
  }

  const quota = formData.team ? quotasQ.data?.find((q) => q.spec.team === formData.team) : undefined
  const teams = [...new Set((quotasQ.data ?? []).map((q) => q.spec.team).filter(Boolean))].sort((a, b) => a.localeCompare(b))
  const gpuTypes = gpuTypeOptions(nodesQ.data, quota, formData.gpuType)
  const dirty = formIsDirty(formData, initial)

  useEffect(() => {
    if (!dirty) return
    const warn = (e: BeforeUnloadEvent) => {
      e.preventDefault()
      e.returnValue = ''
    }
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [dirty])

  const preflightMutation = useMutation({
    mutationFn: async ({ job, snapshot }: { job: Partial<GryviaAIJob>; snapshot: string }) => ({
      report: await api.preflightJob(job), snapshot,
    }),
  })
  const currentPreview = preflightMutation.data?.snapshot === JSON.stringify(formData) ? preflightMutation.data.report : undefined

  const createJobMutation = useMutation({
    mutationFn: (job: Partial<GryviaAIJob>) => api.createJob(job),
    onSuccess: (_created, job) => {
      const jobName = job.metadata?.name ?? formData.name
      notify.success(`Submitted job ${jobName}`)
      queryClient.invalidateQueries({ queryKey: ['jobs'] })
      navigate(`/jobs/${jobName}`)
    },
    onError: (err) => notify.error('Could not submit the job', err),
  })

  const [envIdCounter, setEnvIdCounter] = useState(0)
  const [error, setError] = useState<string | null>(null)

  const duplicate = nameTaken(formData.name, jobsQ.data)
  const nameProblem = (nameTouched ? nameError(formData.name) : undefined) ?? (duplicate ? `A job named ${formData.name} already exists. Pick another name.` : undefined)
  const envProblems = formData.env.map((row, i) => {
    if (row.name.trim() === '') return 'Name is required.'
    if (!ENV_NAME.test(row.name)) return 'Use letters, digits and underscores; do not start with a digit.'
    if (formData.env.some((other, j) => j !== i && other.name === row.name)) return 'Duplicate name.'
    return undefined
  })
  const priorityProblem = priorityError(formData.priority)
  const timeoutProblem = timeoutError(formData.timeout)
  const retryProblem = retryLimitError(formData.retryLimit)
  const totalGpus = formData.distributedEnabled ? formData.nodes * formData.gpusPerNode : formData.gpuCount
  const stats = statsQ.data
  const hints = capacityHints(totalGpus, stats ? { totalGPUs: stats.totalGPUs, availableGPUs: stats.availableGPUs } : undefined, quota, formData.gpuType)
  const argv = argvPreview(formData.command)

  const setTeam = (team: string) => {
    const nextQuota = team ? quotasQ.data?.find((q) => q.spec.team === team) : undefined
    const options = gpuTypeOptions(nodesQ.data, nextQuota)
    setFormData({ ...formData, team, gpuType: options.includes(formData.gpuType) ? formData.gpuType : (options[0] ?? formData.gpuType) })
  }

  const cancel = () => (dirty ? setConfirmLeave(true) : navigate('/jobs'))

  const handleSubmit = (e: React.SyntheticEvent, preview = false) => {
    e.preventDefault()
    setError(null)

    setNameTouched(true)
    const nameProblem = nameError(formData.name)
    if (nameProblem) {
      setError(nameProblem)
      return
    }
    if (duplicate) {
      setError(`A job named ${formData.name} already exists. Pick another name.`)
      return
    }
    if (priorityProblem || timeoutProblem || retryProblem) {
      setError(priorityProblem ?? timeoutProblem ?? retryProblem ?? null)
      return
    }
    if (envProblems.some(Boolean)) {
      setError('Fix the environment variable errors below before submitting.')
      return
    }

    const job: Partial<GryviaAIJob> = {
      apiVersion: 'gryvia.io/v1alpha1',
      kind: 'GryviaAIJob',
      metadata: {
        name: formData.name,
        labels: { [FRAMEWORK_LABEL]: formData.framework, ...(formData.team ? { [TEAM_LABEL]: formData.team } : {}) },
      },
      spec: {
        type: formData.type,
        image: formData.image,
        gpus: totalGpus,
        gpuType: formData.gpuType,
        command: splitCommand(formData.command),
        resources: {
          requests: { cpu: String(formData.cpu), memory: formData.memory },
        },
        distributed: formData.distributedEnabled ? {
          enabled: true,
          framework: formData.framework,
          nodes: formData.nodes,
          gpusPerNode: formData.gpusPerNode,
        } : undefined,
        env: formData.env.length > 0 ? formData.env.map(({ name, value }) => ({ name, value })) : undefined,
        ...(formData.priority.trim() !== '' ? { priority: Number(formData.priority) } : {}),
        ...(formData.timeout.trim() !== '' ? { timeout: formData.timeout.trim() } : {}),
        ...(formData.retryLimit.trim() !== '' ? { retryLimit: Number(formData.retryLimit) } : {}),
      } as GryviaAIJob['spec'],
    }

    if (preview) preflightMutation.mutate({ job, snapshot: JSON.stringify(formData) })
    else createJobMutation.mutate(job)
  }

  const addEnvVar = () => {
    setEnvIdCounter(prev => prev + 1)
    setFormData({
      ...formData,
      env: [...formData.env, { id: envIdCounter + 1000, name: '', value: '', secret: false }],
    })
  }

  const removeEnvVar = (index: number) => {
    setFormData({
      ...formData,
      env: formData.env.filter((_, i) => i !== index),
    })
  }

  const updateEnvVar = (index: number, field: 'name' | 'value', value: string) => {
    setFormData({
      ...formData,
      env: formData.env.map((row, i) => (i === index ? { ...row, [field]: value } : row)),
    })
  }

  const setEnvSecret = (index: number, secret: boolean) => {
    setFormData({ ...formData, env: formData.env.map((row, i) => (i === index ? { ...row, secret } : row)) })
  }

  const toggleShown = (id: number) =>
    setShown((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })

  return (
    <>
      <PageHero eyebrow="Submit job" title="Launch a job in seconds." lede="Configure an AI training or inference job and submit it to the cluster." />

      {cloneName && cloneQ.isLoading && <Skeleton rows={1} />}
      {cloneName && (
        <p className="muted" role="status">
          {cloneQ.isLoading
            ? `Loading ${cloneName} to clone…`
            : cloneQ.data
              ? `Cloned from ${cloneName}. Choose a new name before submitting.${skippedEnv > 0 ? ` ${skippedEnv} variable${skippedEnv === 1 ? '' : 's'} referencing a Secret or ConfigMap ${skippedEnv === 1 ? 'was' : 'were'} not copied.` : ''}`
              : ''}
        </p>
      )}
      {cloneName && cloneQ.isError && (
        <ErrorState title={`Could not load ${cloneName} to clone; the form is blank.`} error={cloneQ.error} onRetry={() => cloneQ.refetch()} retrying={cloneQ.isFetching} />
      )}

      <form onSubmit={handleSubmit} className="grid">
        <section className="card span2">
          <p className="eyebrow">IDENTITY</p>
          <h2 className="card-title">Basic information</h2>
          <div className="formgrid">
            <label className="field">
              <span>Job name</span>
              <input
                type="text"
                required
                value={formData.name}
                onChange={(e) => setFormData({ ...formData, name: e.target.value })}
                onBlur={() => setNameTouched(true)}
                aria-invalid={nameProblem ? true : undefined}
                aria-describedby="job-name-help"
                maxLength={253}
                placeholder="my-training-job"
              />
              <small id="job-name-help" className={nameProblem ? 'warning' : 'faint'} role={nameProblem ? 'alert' : undefined}>
                {nameProblem ?? NAME_HELP}
              </small>
            </label>

            <label className="field">
              <span>Framework</span>
              <select
                value={formData.framework}
                onChange={(e) => setFormData({ ...formData, framework: e.target.value })}
              >
                <option value="pytorch">PyTorch</option>
                <option value="tensorflow">TensorFlow</option>
                <option value="jax">JAX</option>
                <option value="mxnet">MXNet</option>
              </select>
            </label>

            <label className="field">
              <span>Job type</span>
              <select value={formData.type} onChange={(e) => setFormData({ ...formData, type: e.target.value })}>
                {JOB_TYPES.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
                {!(JOB_TYPES as readonly string[]).includes(formData.type) && <option value={formData.type}>{formData.type}</option>}
              </select>
            </label>

            <label className="field">
              <span>Team</span>
              <select value={formData.team} onChange={(e) => setTeam(e.target.value)}>
                <option value={NO_TEAM}>None</option>
                {teams.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
                {formData.team && !teams.includes(formData.team) && <option value={formData.team}>{formData.team}</option>}
              </select>
            </label>

            <label className="field span-all">
              <span>Container image</span>
              <input
                type="text"
                required
                className="mono"
                value={formData.image}
                onChange={(e) => setFormData({ ...formData, image: e.target.value })}
                placeholder="nvcr.io/nvidia/pytorch:24.01-py3"
              />
            </label>
          </div>
        </section>

        <section className="card">
          <p className="eyebrow">RESOURCES</p>
          <h2 className="card-title">Resource requirements</h2>
          <div className="formgrid">
            <label className="field">
              <span>GPU type</span>
              <select
                value={formData.gpuType}
                onChange={(e) => setFormData({ ...formData, gpuType: e.target.value })}
              >
                {gpuTypes.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </select>
            </label>

            <label className="field">
              <span>GPU count</span>
              <input
                type="number"
                min="1"
                max="512"
                required
                disabled={formData.distributedEnabled}
                aria-describedby={formData.distributedEnabled ? 'gpu-total' : undefined}
                value={formData.distributedEnabled ? formData.nodes * formData.gpusPerNode : formData.gpuCount}
                onChange={(e) => setFormData({ ...formData, gpuCount: parseInt(e.target.value, 10) || 1 })}
              />
            </label>

            <div className="span-all stack" aria-live="polite">
              {stats && (
                <small className="faint">
                  Cluster: {stats.availableGPUs} of {stats.totalGPUs} GPUs free.
                  {quota ? ` Team ${quota.spec.team}: up to ${quota.spec.gpuQuota.maxGPUs} GPUs, ${quota.spec.gpuQuota.maxGPUsPerJob} per job.` : ''}
                </small>
              )}
              {hints.map((h) => (
                <small key={h} className="warning">
                  {h}
                </small>
              ))}
            </div>

            <label className="field">
              <span>Memory</span>
              <input
                type="text"
                required
                value={formData.memory}
                onChange={(e) => setFormData({ ...formData, memory: e.target.value })}
                pattern="^\d+(\.\d+)?(Ki|Mi|Gi|Ti|K|M|G|T)?$"
                title="Enter a valid memory value (e.g. 32Gi, 512Mi, 1Ti)"
                placeholder="32Gi"
              />
            </label>

            <label className="field">
              <span>CPU cores</span>
              <input
                type="number"
                min="1"
                max="128"
                required
                value={formData.cpu}
                onChange={(e) => setFormData({ ...formData, cpu: parseInt(e.target.value, 10) || 1 })}
              />
            </label>
          </div>
        </section>

        <section className="card span2">
          <p className="eyebrow">SCALE</p>
          <h2 className="card-title">Distributed training</h2>
          <div className="stack">
            <label className="row">
              <input
                type="checkbox"
                checked={formData.distributedEnabled}
                onChange={(e) => setFormData({ ...formData, distributedEnabled: e.target.checked })}
              />
              <span>Enable distributed training</span>
            </label>

            {formData.distributedEnabled && (
              <p id="gpu-total" className="muted">
                Total GPUs: {formData.nodes} nodes × {formData.gpusPerNode} GPUs per node = <b>{formData.nodes * formData.gpusPerNode}</b>
              </p>
            )}

            {formData.distributedEnabled && (
              <div className="formgrid">
                <label className="field">
                  <span>Nodes</span>
                  <input
                    type="number"
                    min="1"
                    max="64"
                    value={formData.nodes}
                    onChange={(e) => setFormData({ ...formData, nodes: parseInt(e.target.value, 10) || 1 })}
                  />
                </label>
                <label className="field">
                  <span>GPUs per node</span>
                  <input
                    type="number"
                    min="1"
                    max="8"
                    value={formData.gpusPerNode}
                    onChange={(e) => setFormData({ ...formData, gpusPerNode: parseInt(e.target.value, 10) || 1 })}
                  />
                </label>
              </div>
            )}
          </div>
        </section>

        <section className="card">
          <p className="eyebrow">ENTRYPOINT</p>
          <h2 className="card-title" id="command-heading">Command</h2>
          <textarea
            aria-labelledby="command-heading"
            required
            rows={3}
            className="codeedit compact"
            value={formData.command}
            onChange={(e) => setFormData({ ...formData, command: e.target.value })}
            placeholder="python train.py --epochs 100 --batch-size 32"
            aria-describedby="argv-preview"
          />
          <small id="argv-preview" className="faint mono">
            {argv ? `Runs as: ${argv}` : 'Each space-separated word becomes one argument; use quotes to keep spaces.'}
          </small>
        </section>

        <section className="card span3">
          <p className="eyebrow">ADVANCED</p>
          <h2 className="card-title">Scheduling</h2>
          <div className="formgrid">
            <label className="field">
              <span>Priority (0-100)</span>
              <input
                type="number"
                min="0"
                max="100"
                value={formData.priority}
                onChange={(e) => setFormData({ ...formData, priority: e.target.value })}
                aria-invalid={priorityProblem ? true : undefined}
                aria-describedby="priority-help"
                placeholder="default"
              />
              <small id="priority-help" className={priorityProblem ? 'warning' : 'faint'} role={priorityProblem ? 'alert' : undefined}>
                {priorityProblem ?? 'Higher runs first. Leave empty for the default.'}
              </small>
            </label>
            <label className="field">
              <span>Timeout</span>
              <input
                type="text"
                value={formData.timeout}
                onChange={(e) => setFormData({ ...formData, timeout: e.target.value })}
                aria-invalid={timeoutProblem ? true : undefined}
                aria-describedby="timeout-help"
                placeholder="e.g. 2h"
              />
              <small id="timeout-help" className={timeoutProblem ? 'warning' : 'faint'} role={timeoutProblem ? 'alert' : undefined}>
                {timeoutProblem ?? 'Stop the job after this long: 30m, 2h, 7d. Empty means no timeout.'}
              </small>
            </label>
            <label className="field">
              <span>Retry limit (0-10)</span>
              <input
                type="number"
                min="0"
                max="10"
                value={formData.retryLimit}
                onChange={(e) => setFormData({ ...formData, retryLimit: e.target.value })}
                aria-invalid={retryProblem ? true : undefined}
                aria-describedby="retry-help"
                placeholder="default"
              />
              <small id="retry-help" className={retryProblem ? 'warning' : 'faint'} role={retryProblem ? 'alert' : undefined}>
                {retryProblem ?? 'Retries after a failure. Leave empty for the default.'}
              </small>
            </label>
          </div>
        </section>

        <section className="card span3">
          <p className="eyebrow">ENVIRONMENT</p>
          <h2 className="card-title">Environment variables</h2>
          <p className="faint">Values are stored in the job spec in plain text. Put real secrets in Kubernetes Secrets instead; marking a value secret only hides it while you type.</p>
          <div className="toolbar">
            <button type="button" className="btn-secondary" onClick={addEnvVar}>
              Add variable
            </button>
          </div>
          <div className="stack">
            {formData.env.map((env, idx) => (
              <div key={env.id} className="stack">
              <div className="toolbar">
                <input
                  type="text"
                  className="mono"
                  value={env.name}
                  onChange={(e) => updateEnvVar(idx, 'name', e.target.value)}
                  placeholder="VARIABLE_NAME"
                  aria-label={`Variable ${idx + 1} name`}
                  aria-invalid={envProblems[idx] ? true : undefined}
                  aria-describedby={envProblems[idx] ? `env-err-${env.id}` : undefined}
                />
                <input
                  type={env.secret && !shown.has(env.id) ? 'password' : 'text'}
                  className="mono"
                  value={env.value}
                  onChange={(e) => updateEnvVar(idx, 'value', e.target.value)}
                  placeholder="value"
                  autoComplete="off"
                  aria-label={`Variable ${idx + 1} value`}
                />
                <label className="row">
                  <input type="checkbox" checked={env.secret} onChange={(e) => setEnvSecret(idx, e.target.checked)} />
                  <span>Secret</span>
                </label>
                {env.secret && (
                  <button type="button" className="btn-secondary" aria-label={`${shown.has(env.id) ? 'Hide' : 'Show'} value of variable ${idx + 1}`} onClick={() => toggleShown(env.id)}>
                    {shown.has(env.id) ? 'Hide' : 'Show'}
                  </button>
                )}
                <button
                  type="button"
                  className="danger"
                  onClick={() => removeEnvVar(idx)}
                  aria-label={env.name ? `Remove variable ${env.name}` : `Remove variable ${idx + 1}`}
                >
                  <Trash2 className="icon-sm" />
                </button>
              </div>
              {envProblems[idx] && (
                <small id={`env-err-${env.id}`} className="warning" role="alert">
                  {envProblems[idx]}
                </small>
              )}
              </div>
            ))}
          </div>
        </section>

        {error && <p className="warning span3" role="alert">{error}</p>}

        {createJobMutation.isError && (
          <p className="warning span3" role="alert">
            Could not submit the job: {errorMessage(createJobMutation.error)}
          </p>
        )}

        {currentPreview && <div className="span3" role="status">
          <p>Kubernetes admission passed for {currentPreview.namespace}/{currentPreview.name}. No job was created.</p>
          {(currentPreview.warnings ?? []).length > 0 && <>
            <p className="warning">Admission warnings:</p>
            <ul>{currentPreview.warnings?.map((w) => <li key={w}>{w}</li>)}</ul>
          </>}
          <ul>{currentPreview.limitations.map((limit) => <li key={limit}>{limit}</li>)}</ul>
        </div>}
        {preflightMutation.isError && <p className="warning span3" role="alert">
          Admission preview failed: {errorMessage(preflightMutation.error)}
        </p>}
        <div className="toolbar span3">
          <button type="button" className="btn-secondary" onClick={(e) => handleSubmit(e, true)} disabled={preflightMutation.isPending || createJobMutation.isPending}>
            {preflightMutation.isPending ? 'Checking admission…' : 'Check admission'}
          </button>
          <button type="button" className="btn-secondary" onClick={cancel}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={createJobMutation.isPending || preflightMutation.isPending}>
            {createJobMutation.isPending ? 'Submitting…' : 'Submit job'}
          </button>
        </div>
      </form>

      {confirmLeave && (
        <ConfirmDialog title="Discard this job?" confirmLabel="Discard" onCancel={() => setConfirmLeave(false)} onConfirm={() => navigate('/jobs')}>
          You have unsaved changes to this job. Leaving now discards them.
        </ConfirmDialog>
      )}
    </>
  )
}
