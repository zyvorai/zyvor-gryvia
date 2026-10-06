// The operator copilot (POST /api/copilot/chat): plain-language questions answered from read-only cluster tools.

export interface CopilotTurn {
  role: 'user' | 'assistant'
  content: string
  /** Tools the copilot called to answer (assistant turns). */
  tools?: string[]
  error?: string
}

export interface CopilotReply {
  answer: string
  tools: string[]
}

export const MAX_HISTORY = 20

/** The conversation the gateway should see: answered turns only, the most recent MAX_HISTORY. */
export function historyFor(turns: CopilotTurn[]): { role: 'user' | 'assistant'; content: string }[] {
  return turns
    .filter((t) => !t.error && t.content.trim() !== '')
    .slice(-MAX_HISTORY)
    .map((t) => ({ role: t.role, content: t.content }))
}

const TOOL_LABELS: Record<string, string> = {
  list_jobs: 'jobs',
  list_models: 'models',
  list_datasets: 'datasets',
  list_inference_services: 'inference services',
  get_lineage: 'lineage',
  propose_operation: 'operation proposal',
}

/** "Looked at jobs, lineage" for the tools a reply used (each once), or '' when it used none. */
export function toolsLabel(tools: string[] | undefined): string {
  const names = Array.from(new Set((tools ?? []).map((t) => TOOL_LABELS[t] ?? t)))
  return names.length ? `Looked at ${names.join(', ')}` : ''
}

export const EXAMPLES = ['Which jobs failed?', 'Which models are in production and what serves them?', 'What data was this model trained on?']
