import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { notify } from '@/lib/notify'
import type { GryviaAIJob } from '@/types'

const TERMINAL = new Set(['Succeeded', 'Failed', 'Cancelled', 'Rejected', 'Completed'])
const KUEUE_QUEUE = 'kueue.x-k8s.io/queue-name'

/** Workers control for an elastic torchrun job; renders nothing for jobs that cannot be resized. */
export default function ElasticResize({ job }: { job: GryviaAIJob }) {
  const queryClient = useQueryClient()
  const dist = job.spec?.distributed
  const elastic = dist?.elastic
  const max = dist?.nodes ?? 0
  const min = elastic?.minNodes ?? 1
  const desired = elastic?.desiredNodes || max
  const current = job.status?.elastic?.currentNodes
  const [value, setValue] = useState(desired)
  const resize = useMutation({
    mutationFn: (nodes: number) => api.resizeJob(job.metadata.name, nodes),
    onSuccess: (res) => {
      notify.success(`Resizing ${res.name} to ${res.desiredNodes} workers`)
      queryClient.invalidateQueries({ queryKey: ['job', job.metadata.name] })
    },
    onError: (err) => notify.error(`Could not resize ${job.metadata.name}`, err),
  })

  if (!dist?.enabled || !elastic || max < 1) return null
  const phase = job.status?.phase ?? 'Pending'
  const kueue = Boolean(job.metadata.labels?.[KUEUE_QUEUE])
  const blocked = TERMINAL.has(phase) ? `The job is ${phase.toLowerCase()}.` : kueue ? 'Kueue picks the size of a Kueue-managed job.' : ''
  const valid = Number.isInteger(value) && value >= min && value <= max

  return (
    <section className="card">
      <p className="eyebrow">ELASTIC</p>
      <h2 className="card-title">Workers</h2>
      <p className="muted">
        Running {current ?? '—'} of {min}–{max} workers{desired !== current && current !== undefined ? `, resizing to ${desired}` : ''}. Shrinking
        removes the highest-numbered workers; torchrun re-forms the group and resumes from the last checkpoint.
      </p>
      {blocked ? (
        <p className="faint">{blocked}</p>
      ) : (
        <form
          className="toolbar"
          onSubmit={(e) => {
            e.preventDefault()
            if (valid && value !== desired) resize.mutate(value)
          }}
        >
          <label>
            <span className="faint">Desired workers</span>{' '}
            <input type="number" min={min} max={max} step={1} value={value} aria-invalid={!valid} onChange={(e) => setValue(Number(e.target.value))} />
          </label>
          <button type="submit" disabled={!valid || value === desired || resize.isPending} aria-busy={resize.isPending}>
            Resize
          </button>
        </form>
      )}
    </section>
  )
}
