import type { LineageGraph } from './lineage'
import type { AuditResponse } from './audit'
import type { CopilotReply } from './copilot'
import type { CatalogEdit, CatalogEntry, CatalogResponse } from './dataCatalog'
import axios from 'axios'
import toast from 'react-hot-toast'
import { notifyUnauthorized } from '@/lib/authEvents'
import type { GryviaAIJob, GryviaQuota, GryviaGpuNode } from '@/types'
import { getStoredToken, clearToken } from '@/lib/auth'
import type { Reservation, ReservationBody } from '@/lib/reservations'
import type { Budget } from '@/lib/budgets'
import type { Experiment } from '@/lib/experiments'
import type { Sku, SkuBody, TenantResource, CreateTenantBody, UsageReport } from '@/lib/cloud'
import type { Invoice, InvoiceReport } from '@/lib/invoices'
import type { ModelWatch, ModelWatchRun } from '@/lib/modelWatches'
import type { Dataset } from '@/lib/datasets'
import type { CreatedLlmKey, LlmKey, LlmModels, LlmUsage } from '@/lib/llm'
import { parseSSE, readChunk, replyText, type ChatRequestBody, type StreamChunk } from '@/lib/playground'
import type { VectorIndexList } from '@/lib/rag'
import type { Agent, AgentReply, ChatMessage } from '@/lib/agents'
import type { SovereignStatus } from '@/lib/sovereign'

export interface ClusterStats {
  totalGPUs: number
  availableGPUs: number
  allocatedGPUs: number
  utilizationPercent: number
  totalJobs: number
  runningJobs: number
  pendingJobs: number
  completedJobs: number
  failedJobs: number
  totalNodes: number
  avgGPUUtilization?: number
}

export interface GPUMetric {
  node: string
  gpuIndex: number
  utilization: number
  temperature: number
  memoryUsed: number
  memoryTotal: number
  timestamp: string
}

export interface GPUMetricsResponse {
  timeRange: string
  metrics: GPUMetric[]
}

export interface CostData {
  /** 'all-time': the totals cover every billable job since creation, not a calendar month. */
  scope?: string
  monthly: Array<{ month: string; cost: number }>
  hasHistoricalData: boolean
  byTeam: Array<{ team: string; cost: number }>
  byGPUType: Array<{ type: string; cost: number; hours: number }>
  totalCost: number
}

// Network intelligence types
export interface NetworkFlow {
  metadata: { name: string; namespace?: string; creationTimestamp?: string }
  spec: {
    timestamp: string
    source: string
    destination: string
    protocol: string
    port: number
    bytes: string
    latency: string
    verdict: 'FORWARDED' | 'DROP' | 'DENIED' | 'ALLOW' | 'ERROR' | ''
    policy?: string
  }
}

export interface FlowPolicy {
  metadata: { name: string; namespace?: string; creationTimestamp?: string; labels?: Record<string, string> }
  spec: {
    sourceService: string
    destinationService: string
    port: number
    protocol: string
    action: 'allow' | 'deny' | 'log'
    intent: string
    confidence?: number
  }
  status?: {
    phase: 'Enforced' | 'Pending' | 'Suggested' | 'Active'
    matchedFlows: number
    ciliumPolicyRef?: string
  }
}

export interface ServiceGraphNode {
  id: string
  label: string
  health: 'healthy' | 'warning' | 'critical' | 'unknown'
  flowCount: number
}

export interface ServiceGraphEdge {
  source: string
  target: string
  protocol: string
  latency: string
  verdict: string
}

export interface ServiceGraph {
  nodes: ServiceGraphNode[]
  edges: ServiceGraphEdge[]
}

export interface TrafficInsights {
  activeFlows: number
  policiesEnforced: number
  anomaliesDetected: number
  traceSessions: number
}

export interface NetworkAnomaly {
  metadata: { name: string; namespace?: string; creationTimestamp?: string }
  spec: {
    severity: 'critical' | 'high' | 'medium' | 'low'
    service: string
    type: string
    description: string
    detectedAt: string
  }
}

export interface TraceSession {
  metadata: { name: string; namespace?: string; creationTimestamp?: string }
  spec: {
    targetService: string
    duration: string
    captureLevel: string
    namespace: string
  }
  status?: {
    phase: 'Running' | 'Completed' | 'Failed' | 'Active'
    flowsCaptured: number
  }
}

export interface CreateFlowPolicyRequest {
  name?: string
  sourceService: string
  destinationService: string
  port: number
  protocol: string
  action: 'allow' | 'deny' | 'log'
  intent: string
}

export interface CreateTraceSessionRequest {
  targetService: string
  duration: string
  captureLevel: string
  namespace: string
}

// Security types
export interface SecurityAlert {
  type: string
  severity: 'critical' | 'high' | 'medium' | 'low'
  process: string
  path: string
  sourceIP: string
  message: string
  timestamp: string
}

export interface SecurityPolicy {
  metadata: { name: string; namespace?: string; creationTimestamp?: string }
  spec: {
    targetNamespaces: string[]
    detectionRules: Array<{
      type: string
      enabled: boolean
      sensitivity: string
    }>
    alertWebhook?: string
    autoBlock: boolean
  }
  status?: {
    phase: string
    activeDetections: number
    alertsTriggered: number
    lastAlert?: string
    detectionCounts?: Record<string, number>
  }
}

export interface CreateSecurityPolicyRequest {
  name: string
  targetNamespaces: string[]
  detectionRules: Array<{
    type: string
    enabled: boolean
    sensitivity: string
  }>
  autoBlock: boolean
  alertWebhook?: string
}

// Network cost types
export interface NetworkCostReport {
  period: string
  namespace: string
  team: string
  sameZoneBytes: number
  crossZoneBytes: number
  externalBytes: number
  totalCostUSD: number
}

export interface NetworkCostData {
  reports: NetworkCostReport[]
  costPerGB: {
    sameZone: number
    crossZone: number
    internetEgress: number
  }
}

// Training insight types
export interface TrainingInsight {
  source?: { name: string; namespace: string }
  rankStats: Array<{
    rank: number
    avgLatencyNs: number
    totalBytes: number
    isStraggler: boolean
  }>
  commPattern: string
  commComputeRatio: number
  stragglers: Array<{
    rank: number
    slowdownFactor: number
    reason: string
  }>
  bottleneck: string
  lastAnalysis?: string
}

// NCCL stats types
export interface NCCLStats {
  collectors?: { reachable: number; total: number }
  operations: Array<{
    opType: string
    count: number
    avgLatencyNs: number
    p99LatencyNs: number
    totalBytes: number
  }>
}

// GPU memory stats types
export interface GPUMemStats {
  collectors?: { reachable: number; total: number }
  h2dBytes: number
  d2hBytes: number
  d2dBytes: number
  h2dCount: number
  d2hCount: number
  d2dCount: number
}

// Workspace types
export interface Workspace {
  metadata?: { name?: string; namespace?: string; creationTimestamp?: string }
  spec?: {
    type?: string
    gpuCount?: number
    gpuType?: string
    storageSize?: string
    idleTimeout?: string
  }
  status?: {
    phase?: string
    url?: string
    uptime?: string
    lastActivity?: string
  }
}

export interface CreateWorkspaceRequest {
  name: string
  type: string
  gpuCount: number
  gpuType: string
  storageSize: string
  idleTimeout: string
}

// Model registry types
export interface RegisteredModel {
  metadata?: { name?: string; namespace?: string; creationTimestamp?: string }
  /** A tenant's request to promote, waiting for an administrator (the gateway runs with GRYVIA_REQUIRE_PROD_APPROVAL=1). */
  pendingPromotion?: { target: string; by: string; at: string } | null
  spec?: {
    version?: string
    stage?: string
    sourceJob?: string
    artifacts?: string[]
  }
  status?: {
    servingEndpoint?: string
  }
}

// Inference service types
export interface InferenceService {
  metadata?: { name?: string; namespace?: string; creationTimestamp?: string }
  spec?: {
    modelRef?: string
    backend?: string
    replicas?: number
    canary?: {
      trafficPercent?: number
    }
    autoscaling?: {
      minReplicas?: number
      maxReplicas?: number
      targetUtilization?: number
    }
  }
  status?: {
    phase?: string
    readyReplicas?: number
    endpoint?: string
  }
}

export interface CreateInferenceServiceRequest {
  name: string
  modelRef: string
  backend: string
  replicas: number
  minReplicas: number
  maxReplicas: number
  targetUtilization: number
}

// Workflow types
export interface WorkflowStep {
  name: string
  type?: string
  status?: string
  duration?: string
  jobRef?: string
  dependsOn?: string[]
}

export interface Workflow {
  metadata?: { name?: string; namespace?: string; creationTimestamp?: string }
  spec?: {
    steps?: WorkflowStep[]
  }
  status?: {
    phase?: string
    steps?: WorkflowStep[]
    duration?: string
    startedAt?: string
  }
}

// Auto tuner types
export interface TunerTrial {
  trialId: string
  parameters?: Record<string, unknown>
  metricValue?: number
  status?: string
  duration?: string
}

export interface AutoTunerJob {
  metadata?: { name?: string; namespace?: string; creationTimestamp?: string }
  spec?: {
    algorithm?: string
    objectiveMetric?: string
    metricName?: string
    direction?: 'maximize' | 'minimize'
    maxTrials?: number
    parameterSpace?: Record<string, unknown>
  }
  status?: {
    phase?: string
    trialsCompleted?: number
    trialsRunning?: number
    bestMetricValue?: number
    bestTrialId?: string
  }
}

// Job runtime details (pods, logs, events)
export interface JobResize {
  name: string
  namespace: string
  desiredNodes: number
  minNodes: number
  maxNodes: number
  currentNodes?: number | null
}

export interface JobPod {
  name: string
  phase: string
  node?: string
  podIP?: string
  startTime?: string
  restarts: number
  message?: string
  containers: Array<{ name: string; ready: boolean; state: string; restartCount: number; reason?: string }>
}

export interface JobLogs {
  pod: string | null
  container?: string
  lines: string[]
  truncated: boolean
}

export interface JobEvent {
  type: string
  reason: string
  message: string
  count: number
  firstSeen?: string
  lastSeen?: string
  object: string
}

export interface CreateWorkflowRequest {
  metadata: { name: string }
  /** Steps as accepted by the gateway: name, type (job|script|webhook), dependsOn, and the step payload. */
  spec: { steps: unknown[] }
}

export interface CreateTunerRequest {
  name: string
  algorithm: string
  objectiveMetric: string
  direction: 'maximize' | 'minimize'
  maxTrials: number
  parameterSpace: string
  /** The job run for each trial (required by the GryviaAutoTuner CRD). */
  jobTemplate: { type: 'training'; image: string; gpus: number }
  /** Required when algorithm is ASHA. */
  ashaConfig?: { maxEpochs: number }
}

// Flight Recorder: the gateway's merged cluster view of the per-node collectors.
export interface FlightEvent {
  time: string
  node: string
  kind: string
  source?: string
  operation?: string
  bytes?: number
  retransmits?: number
  duration_ns?: number
  identity: { namespace?: string; job?: string; pod?: string; node?: string; rank?: string }
}

export interface FlightReport {
  namespace: string
  job: string
  scope?: string
  /** complete: every DISCOVERED collector answered (collectors that are not Running are not counted). */
  coverage: { total: number; reachable: number; reporting: number; complete: boolean }
  /** True when events were dropped by a per-node or overall limit. */
  truncated: boolean
  nodes: string[]
  counts: Record<string, number>
  findings: Array<{ node: string; code: string; evidence: string }>
  rankObservations?: Array<{ operation: string; rank: string; samples: number; medianDurationNs: number }>
  events: FlightEvent[]
}

// Unified diagnosis: the gateway's merge of the per-node evidence-backed diagnoses.
export interface DiagnosisEvidence {
  node?: string
  source: string
  metric: string
  value: number
  window: string
}

export interface DiagnosisFinding {
  kind: string
  severity: 'info' | 'warning' | 'critical'
  confidence: 'low' | 'medium' | 'high'
  nodes: string[]
  evidence: DiagnosisEvidence[]
  summary: string
  whatWasNotMeasured: string[]
}

export interface MeasurementCompleteness {
  probesAttached: number
  probesSkipped: Array<{ node?: string; object: string; program?: string; reason?: string }>
  droppedEvents: { total: number; byNode?: Record<string, number> }
  sampling: { ratio: number; note?: string }
  /** A number, or the string "unknown" when the gateway could not list the job's pods. */
  nodesExpected: number | 'unknown'
  nodesReporting: number
  missingNodes: string[]
  complete: boolean
  reasons: string[]
}

export interface FlightDiagnosis {
  namespace: string
  job: string
  summary: string
  partial: boolean
  coverage: { total: number; reachable: number; reporting: number; complete: boolean }
  nodes: string[]
  findings: DiagnosisFinding[]
  unavailable: Array<{ node: string; signal: string; reason: string }>
  measured: Record<string, string[]>
  measurementCompleteness: MeasurementCompleteness
}

// Serving-engine latency of a job (vLLM, Triton, TGI), read from GryviaFabricSignal.status by the gateway.
// Every figure is optional: a missing key means "not measured", never zero. Latencies are milliseconds.
export interface InferenceLatency {
  namespace: string
  job: string
  /** False unless the collector's opt-in metrics scraper published an engine. */
  available: boolean
  engine?: string
  updatedAt?: string
  ttftP99ms?: number
  itlP99ms?: number
  queueTimeP99ms?: number
  e2eP99ms?: number
  /** Means, for engines that export only duration counters (Triton without summaries). */
  queueTimeMeanMs?: number
  e2eMeanMs?: number
  requestsWaiting?: number
  kvCacheUsage?: number
  /** Network accept -> first recv wait (eBPF). Not engine queue time. */
  inferWaitP99ms?: number
}

const apiClient = axios.create({
  baseURL: '/api',
  timeout: 30000,
  headers: {
    'Content-Type': 'application/json',
  },
})

// Attach auth token to all requests
apiClient.interceptors.request.use((config) => {
  const token = getStoredToken() || ''
  if (token) {
    config.headers.Authorization = `Bearer ${token}`
  }
  return config
})

// Session and rate-limit handling. 401 goes through the router (keeps the return path and shows
// "session expired"); 403/429 tell the user instead of failing silently.
apiClient.interceptors.response.use(
  (response) => response,
  (error) => {
    const status = error.response?.status
    if (status === 401) {
      clearToken()
      const path = window.location.pathname
      if (path !== '/login' && path !== '/auth/callback' && !notifyUnauthorized()) {
        window.location.href = '/login'
      }
    } else if (status === 403) {
      toast.error("You don't have permission to do that.", { id: 'http-403' })
    } else if (status === 429) {
      toast.error('Too many requests. Wait a moment and try again.', { id: 'http-429' })
    }
    return Promise.reject(error)
  }
)

export const api = {
  // Cluster stats
  getClusterStats: async (): Promise<ClusterStats> => {
    const { data } = await apiClient.get('/cluster/stats')
    return data
  },

  // Jobs - routed through API gateway
  getJobs: async (): Promise<GryviaAIJob[]> => {
    const { data } = await apiClient.get('/jobs')
    return data.items || []
  },

  getJob: async (name: string): Promise<GryviaAIJob> => {
    const { data } = await apiClient.get(`/jobs/${encodeURIComponent(name)}`)
    return data
  },

  preflightJob: async (job: Partial<GryviaAIJob>): Promise<{ admitted: boolean; persisted: boolean; namespace: string; name: string; enforcedOnCreate?: boolean; warnings?: string[]; limitations: string[] }> => {
    const { data } = await apiClient.post('/jobs/preflight', job)
    return data
  },

  createJob: async (job: Partial<GryviaAIJob>): Promise<GryviaAIJob> => {
    const { data } = await apiClient.post('/jobs', job)
    return data
  },

  getJobPods: async (name: string): Promise<JobPod[]> => {
    const { data } = await apiClient.get(`/jobs/${encodeURIComponent(name)}/pods`)
    return data.items || []
  },

  getJobLogs: async (name: string, opts: { pod?: string; tail?: number } = {}): Promise<JobLogs> => {
    const { data } = await apiClient.get(`/jobs/${encodeURIComponent(name)}/logs`, { params: { pod: opts.pod, tail: opts.tail } })
    return data
  },

  getJobEvents: async (name: string): Promise<JobEvent[]> => {
    const { data } = await apiClient.get(`/jobs/${encodeURIComponent(name)}/events`)
    return data.items || []
  },

  resizeJob: async (name: string, nodes: number): Promise<JobResize> => {
    const { data } = await apiClient.post(`/jobs/${encodeURIComponent(name)}/resize`, { nodes })
    return data
  },

  deleteJob: async (name: string): Promise<void> => {
    await apiClient.delete(`/jobs/${encodeURIComponent(name)}`)
  },

  // Quotas - routed through API gateway
  getQuotas: async (): Promise<GryviaQuota[]> => {
    const { data } = await apiClient.get('/quotas')
    return data.items || []
  },

  getQuota: async (name: string): Promise<GryviaQuota> => {
    const { data } = await apiClient.get(`/quotas/${encodeURIComponent(name)}`)
    return data
  },

  // Nodes - routed through API gateway
  getNodes: async (): Promise<GryviaGpuNode[]> => {
    const { data } = await apiClient.get('/nodes')
    return data.items || []
  },

  getNode: async (name: string): Promise<GryviaGpuNode> => {
    const { data } = await apiClient.get(`/nodes/${encodeURIComponent(name)}`)
    return data
  },

  // GPU Metrics
  getGPUMetrics: async (): Promise<GPUMetricsResponse> => {
    const { data } = await apiClient.get('/metrics/gpu')
    return data
  },

  // Cost data
  getCostData: async (): Promise<CostData> => {
    const { data } = await apiClient.get('/metrics/costs')
    return data
  },

  // Network intelligence
  getFlowPolicies: async (): Promise<FlowPolicy[]> => {
    const { data } = await apiClient.get('/network/policies')
    return data.items || []
  },

  createFlowPolicy: async (policy: CreateFlowPolicyRequest): Promise<FlowPolicy> => {
    const { data } = await apiClient.post('/network/policies', policy)
    return data
  },

  applyFlowPolicy: async (name: string): Promise<void> => {
    await apiClient.post(`/network/policies/${encodeURIComponent(name)}/apply`)
  },

  getTrafficInsights: async (): Promise<TrafficInsights> => {
    const { data } = await apiClient.get('/network/insights')
    return data
  },

  getServiceGraph: async (): Promise<ServiceGraph> => {
    const { data } = await apiClient.get('/network/graph')
    return data
  },

  getNetworkAnomalies: async (): Promise<NetworkAnomaly[]> => {
    const { data } = await apiClient.get('/network/anomalies')
    return data.items || []
  },

  getNetworkFlows: async (): Promise<NetworkFlow[]> => {
    const { data } = await apiClient.get('/network/flows')
    return data.items || []
  },

  createTraceSession: async (req: CreateTraceSessionRequest): Promise<TraceSession> => {
    const { data } = await apiClient.post('/network/traces', req)
    return data
  },

  getTraceSessions: async (): Promise<TraceSession[]> => {
    const { data } = await apiClient.get('/network/traces')
    return data.items || []
  },

  getTraceSession: async (name: string): Promise<TraceSession> => {
    const { data } = await apiClient.get(`/network/traces/${encodeURIComponent(name)}`)
    return data
  },

  // Security
  getSecurityAlerts: async (): Promise<SecurityAlert[]> => {
    const { data } = await apiClient.get('/security/alerts')
    return data.items || []
  },

  /** Alerts plus whether any event source is connected (false = an empty list does not mean "all clear"). */
  getSecurityAlertsStatus: async (): Promise<{ items: SecurityAlert[]; eventSource: boolean }> => {
    const { data } = await apiClient.get('/security/alerts')
    return { items: data.items || [], eventSource: data.eventSource !== false }
  },

  getSecurityPolicies: async (): Promise<SecurityPolicy[]> => {
    const { data } = await apiClient.get('/security/policies')
    return data.items || []
  },

  createSecurityPolicy: async (policy: CreateSecurityPolicyRequest): Promise<SecurityPolicy> => {
    const { data } = await apiClient.post('/security/policies', policy)
    return data
  },

  // Network costs
  getNetworkCosts: async (): Promise<NetworkCostData> => {
    const { data } = await apiClient.get('/network/costs')
    return data
  },

  // Training insights
  getTrainingInsight: async (): Promise<TrainingInsight> => {
    const { data } = await apiClient.get('/ai/training/insight')
    return data
  },

  // NCCL stats
  getNCCLStats: async (): Promise<NCCLStats> => {
    const { data } = await apiClient.get('/ai/training/nccl')
    return data
  },

  // GPU memory stats
  getGPUMemoryStats: async (): Promise<GPUMemStats> => {
    const { data } = await apiClient.get('/gpu/memory')
    return data
  },

  // GPU-as-a-Service: catalog, tenants, usage
  getSkus: async (): Promise<Sku[]> => {
    const { data } = await apiClient.get('/skus')
    return data.items || []
  },

  createSku: async (body: SkuBody & { name: string }): Promise<Sku> => {
    const { data } = await apiClient.post('/skus', body)
    return data
  },

  updateSku: async (name: string, body: SkuBody): Promise<Sku> => {
    const { data } = await apiClient.put(`/skus/${encodeURIComponent(name)}`, body)
    return data
  },

  deleteSku: async (name: string): Promise<void> => {
    await apiClient.delete(`/skus/${encodeURIComponent(name)}`)
  },

  getTenants: async (): Promise<TenantResource[]> => {
    const { data } = await apiClient.get('/tenants')
    return data.items || []
  },

  createTenant: async (body: CreateTenantBody): Promise<TenantResource> => {
    const { data } = await apiClient.post('/tenants', body)
    return data
  },

  deleteTenant: async (name: string): Promise<void> => {
    await apiClient.delete(`/tenants/${encodeURIComponent(name)}`)
  },

  getUsage: async (params: Record<string, string>): Promise<UsageReport> => {
    const { data } = await apiClient.get('/usage', { params })
    return data
  },

  /** Authenticated download (the token is a bearer header, so a plain link would be rejected). */
  exportUsage: async (format: 'csv' | 'json', params: Record<string, string>): Promise<Blob> => {
    const { data } = await apiClient.get('/usage/export', { params: { ...params, format }, responseType: 'blob' })
    return data as Blob
  },

  getReservations: async (): Promise<Reservation[]> => {
    const { data } = await apiClient.get('/reservations')
    return data.items || []
  },

  createReservation: async (body: ReservationBody): Promise<Reservation> => {
    const { data } = await apiClient.post('/reservations', body)
    return data
  },

  cancelReservation: async (name: string): Promise<void> => {
    await apiClient.delete(`/reservations/${encodeURIComponent(name)}`)
  },

  getExperiments: async (): Promise<Experiment[]> => {
    const { data } = await apiClient.get('/experiments')
    return data.items || []
  },

  getBudgets: async (): Promise<Budget[]> => {
    const { data } = await apiClient.get('/budgets')
    return data.items || []
  },

  getFlightReport: async (job: string, namespace: string): Promise<FlightReport> => {
    const { data } = await apiClient.get(`/flight/jobs/${encodeURIComponent(job)}`, { params: { namespace } })
    return data
  },

  getFlightDiagnosis: async (job: string, namespace: string): Promise<FlightDiagnosis> => {
    const { data } = await apiClient.get(`/flight/jobs/${encodeURIComponent(job)}/diagnosis`, { params: { namespace } })
    return data
  },

  getInferenceLatency: async (job: string, namespace: string): Promise<InferenceLatency> => {
    const { data } = await apiClient.get(`/flight/inference/${encodeURIComponent(job)}`, { params: { namespace } })
    return data
  },

  getInvoices: async (params: Record<string, string>): Promise<InvoiceReport> => {
    const { data } = await apiClient.get('/invoices', { params })
    return data
  },

  /** Freeze a closed month into a numbered GryviaInvoice from the billing ledger (admin). */
  finalizeInvoice: async (tenant: string, month: string): Promise<Invoice> => {
    const { data } = await apiClient.post(`/invoices/${encodeURIComponent(tenant)}/${encodeURIComponent(month)}/finalize`)
    return data
  },

  /** Void a finalized invoice; it is kept for audit and the month can be finalized again (admin). */
  voidInvoice: async (tenant: string, month: string, reason: string): Promise<Invoice> => {
    const { data } = await apiClient.post(`/invoices/${encodeURIComponent(tenant)}/${encodeURIComponent(month)}/void`, { reason })
    return data
  },

  /** Authenticated invoice download (bearer header, so not a plain link). */
  downloadInvoice: async (tenant: string, month: string, format: 'csv' | 'json'): Promise<Blob> => {
    const { data } = await apiClient.get(`/invoices/${encodeURIComponent(tenant)}/${encodeURIComponent(month)}`, { params: { format }, responseType: 'blob' })
    return data as Blob
  },

  // Workspaces
  getWorkspaces: async (): Promise<Workspace[]> => {
    const { data } = await apiClient.get('/workspaces')
    return data.items || []
  },

  getWorkspace: async (name: string): Promise<Workspace> => {
    const { data } = await apiClient.get(`/workspaces/${encodeURIComponent(name)}`)
    return data
  },

  createWorkspace: async (req: CreateWorkspaceRequest): Promise<Workspace> => {
    const { data } = await apiClient.post('/workspaces', req)
    return data
  },

  pauseWorkspace: async (name: string): Promise<void> => {
    await apiClient.post(`/workspaces/${encodeURIComponent(name)}/pause`)
  },

  resumeWorkspace: async (name: string): Promise<void> => {
    await apiClient.post(`/workspaces/${encodeURIComponent(name)}/resume`)
  },

  deleteWorkspace: async (name: string): Promise<void> => {
    await apiClient.delete(`/workspaces/${encodeURIComponent(name)}`)
  },

  // Model Registry
  getModels: async (): Promise<RegisteredModel[]> => {
    const { data } = await apiClient.get('/models')
    return data.items || []
  },

  getModel: async (name: string): Promise<RegisteredModel> => {
    const { data } = await apiClient.get(`/models/${encodeURIComponent(name)}`)
    return data
  },

  promoteModel: async (name: string, targetStage: string): Promise<RegisteredModel> => {
    const { data } = await apiClient.post(`/models/${encodeURIComponent(name)}/promote`, { targetStage })
    return data
  },

  approveModelPromotion: async (name: string): Promise<RegisteredModel> => {
    const { data } = await apiClient.post(`/models/${encodeURIComponent(name)}/approve`)
    return data
  },

  rejectModelPromotion: async (name: string): Promise<RegisteredModel> => {
    const { data } = await apiClient.post(`/models/${encodeURIComponent(name)}/reject`)
    return data
  },

  // Inference Services
  getInferenceServices: async (): Promise<InferenceService[]> => {
    const { data } = await apiClient.get('/inference')
    return data.items || []
  },

  getInferenceService: async (name: string): Promise<InferenceService> => {
    const { data } = await apiClient.get(`/inference/${encodeURIComponent(name)}`)
    return data
  },

  createInferenceService: async (req: CreateInferenceServiceRequest): Promise<InferenceService> => {
    const { data } = await apiClient.post('/inference', req)
    return data
  },

  deleteInferenceService: async (name: string): Promise<void> => {
    await apiClient.delete(`/inference/${encodeURIComponent(name)}`)
  },

  // Workflows
  getWorkflows: async (): Promise<Workflow[]> => {
    const { data } = await apiClient.get('/workflows')
    return data.items || []
  },

  getWorkflow: async (name: string): Promise<Workflow> => {
    const { data } = await apiClient.get(`/workflows/${encodeURIComponent(name)}`)
    return data
  },

  deleteWorkflow: async (name: string): Promise<void> => {
    await apiClient.delete(`/workflows/${encodeURIComponent(name)}`)
  },

  createWorkflow: async (workflow: CreateWorkflowRequest): Promise<Workflow> => {
    const { data } = await apiClient.post('/workflows', workflow)
    return data
  },

  // Auto Tuners
  getTuners: async (): Promise<AutoTunerJob[]> => {
    const { data } = await apiClient.get('/tuners')
    return data.items || []
  },

  getTuner: async (name: string): Promise<AutoTunerJob> => {
    const { data } = await apiClient.get(`/tuners/${encodeURIComponent(name)}`)
    return data
  },

  deleteTuner: async (name: string): Promise<void> => {
    await apiClient.delete(`/tuners/${encodeURIComponent(name)}`)
  },

  createTuner: async (req: CreateTunerRequest): Promise<AutoTunerJob> => {
    const { data } = await apiClient.post('/tuners', req)
    return data
  },

  getTunerTrials: async (name: string): Promise<TunerTrial[]> => {
    const { data } = await apiClient.get(`/tuners/${encodeURIComponent(name)}/trials`)
    return data.items || []
  },

  // Model watches (model factory)
  getModelWatches: async (): Promise<ModelWatch[]> => {
    const { data } = await apiClient.get('/model-watches')
    return data.items || []
  },

  getModelWatchRuns: async (name: string): Promise<ModelWatchRun[]> => {
    const { data } = await apiClient.get(`/model-watches/${encodeURIComponent(name)}/runs`)
    return data.items || []
  },

  suspendModelWatch: async (name: string): Promise<void> => {
    await apiClient.post(`/model-watches/${encodeURIComponent(name)}/suspend`)
  },

  resumeModelWatch: async (name: string): Promise<void> => {
    await apiClient.post(`/model-watches/${encodeURIComponent(name)}/resume`)
  },

  deleteModelWatch: async (name: string): Promise<void> => {
    await apiClient.delete(`/model-watches/${encodeURIComponent(name)}`)
  },

  // Datasets
  getDatasets: async (): Promise<Dataset[]> => {
    const { data } = await apiClient.get('/datasets')
    return data.items || []
  },

  deleteDataset: async (name: string): Promise<void> => {
    await apiClient.delete(`/datasets/${encodeURIComponent(name)}`)
  },

  // LLM gateway
  getLlmModels: async (): Promise<LlmModels> => {
    const { data } = await apiClient.get('/llm/models')
    return { items: data.items || [], gatewayURL: data.gatewayURL || '', enabled: !!data.enabled }
  },

  getLlmUsage: async (groupBy: LlmUsage['groupBy'] = 'model'): Promise<LlmUsage> => {
    const { data } = await apiClient.get('/llm/usage', { params: { groupBy } })
    return data
  },

  getLlmKeys: async (): Promise<LlmKey[]> => {
    const { data } = await apiClient.get('/llm-keys')
    return data.items || []
  },

  createLlmKey: async (body: { name: string; namespace?: string; description?: string }): Promise<CreatedLlmKey> => {
    const { data } = await apiClient.post('/llm-keys', body)
    return data
  },

  deleteLlmKey: async (id: string): Promise<void> => {
    await apiClient.delete(`/llm-keys/${encodeURIComponent(id)}`)
  },

  // Vector indexes (RAG)
  getVectorIndexes: async (): Promise<VectorIndexList> => {
    const { data } = await apiClient.get('/vector-indexes')
    return { items: data.items || [], retrieveURL: data.retrieveURL || '' }
  },

  reingestVectorIndex: async (name: string): Promise<void> => {
    await apiClient.post(`/vector-indexes/${encodeURIComponent(name)}/reingest`)
  },

  suspendVectorIndex: async (name: string, suspend: boolean): Promise<void> => {
    await apiClient.post(`/vector-indexes/${encodeURIComponent(name)}/${suspend ? 'suspend' : 'resume'}`)
  },

  deleteVectorIndex: async (name: string): Promise<void> => {
    await apiClient.delete(`/vector-indexes/${encodeURIComponent(name)}`)
  },

  // Model playground: the caller's LLM key goes in X-LLM-Key and is not kept anywhere but the page's memory.
  llmChat: async (body: ChatRequestBody, key: string): Promise<{ text: string; usage?: StreamChunk['usage'] }> => {
    const { data } = await apiClient.post('/llm/chat', { ...body, stream: false }, { headers: { 'X-LLM-Key': key }, timeout: 300_000 })
    return { text: replyText(data), usage: data.usage }
  },

  /** Streams a chat completion, calling onChunk for each event; axios cannot read a response stream in the browser. */
  streamLlmChat: async (body: ChatRequestBody, key: string, onChunk: (c: StreamChunk) => void, signal?: AbortSignal): Promise<void> => {
    const token = getStoredToken() || ''
    const resp = await fetch('/api/llm/chat', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-LLM-Key': key, ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      body: JSON.stringify({ ...body, stream: true }),
      signal,
    })
    if (!resp.ok) {
      const detail = await resp.json().then((d) => d?.detail, () => undefined)
      throw new Error(resp.status === 401 ? 'Your session expired; sign in again.' : `${resp.status}: ${detail || resp.statusText}`)
    }
    if (!(resp.headers.get('content-type') || '').startsWith('text/event-stream') || !resp.body) {
      const data = await resp.json()
      onChunk({ delta: replyText(data), usage: data.usage })
      return
    }
    const reader = resp.body.getReader()
    const decoder = new TextDecoder()
    let buffer = ''
    for (;;) {
      const { value, done } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })
      const parsed = parseSSE(buffer)
      buffer = parsed.rest
      for (const d of parsed.data) {
        const c = readChunk(d)
        if (c.done) return
        onChunk(c)
      }
    }
  },

  getAudit: async (params: Record<string, string | number>): Promise<AuditResponse> => {
    const { data } = await apiClient.get('/audit', { params })
    return data
  },
  exportAudit: async (params: Record<string, string | number>): Promise<Blob> => {
    const { data } = await apiClient.get('/audit/export.csv', { params, responseType: 'blob' })
    return data
  },

  copilotChat: async (body: { model: string; question: string; history: { role: string; content: string }[] }, key: string): Promise<CopilotReply> => {
    const { data } = await apiClient.post('/copilot/chat', body, { headers: { 'X-LLM-Key': key }, timeout: 300_000 })
    return data
  },

  getDataCatalog: async (params: { q?: string; tag?: string; owner?: string }): Promise<CatalogResponse> => {
    const { data } = await apiClient.get('/data-catalog', { params: Object.fromEntries(Object.entries(params).filter(([, v]) => v)) })
    return data
  },
  editCatalogEntry: async (name: string, body: CatalogEdit): Promise<CatalogEntry> => {
    const { data } = await apiClient.put(`/data-catalog/${encodeURIComponent(name)}`, body)
    return data
  },

  getLineage: async (): Promise<LineageGraph> => {
    const { data } = await apiClient.get('/lineage')
    return data
  },

  // Sovereign AI OS: Zyntra and Netra as the gateway sees them
  getSovereign: async (): Promise<SovereignStatus> => {
    const { data } = await apiClient.get('/sovereign')
    return data
  },

  // Agents
  getAgents: async (): Promise<Agent[]> => {
    const { data } = await apiClient.get('/agents')
    return data.items || []
  },

  chatAgent: async (name: string, messages: ChatMessage[]): Promise<AgentReply> => {
    // An agent turn runs several model calls and tools.
    const { data } = await apiClient.post(`/agents/${encodeURIComponent(name)}/chat`, { messages }, { timeout: 180_000 })
    return data
  },

  scaleAgent: async (name: string, replicas: number): Promise<void> => {
    await apiClient.post(`/agents/${encodeURIComponent(name)}/scale`, { replicas })
  },

  deleteAgent: async (name: string): Promise<void> => {
    await apiClient.delete(`/agents/${encodeURIComponent(name)}`)
  },
}

export default apiClient
