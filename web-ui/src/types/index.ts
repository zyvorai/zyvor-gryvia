export interface GryviaAIJob {
  apiVersion: string
  kind: string
  metadata: {
    name: string
    namespace?: string
    creationTimestamp?: string
    labels?: Record<string, string>
  }
  spec: {
    /** training | inference | fine-tuning | evaluation */
    type: string
    image: string
    /** Total GPUs for the job. */
    gpus: number
    gpuType?: string
    model?: string
    command?: string[]
    args?: string[]
    env?: Array<{ name: string; value: string }>
    distributed?: {
      enabled?: boolean
      framework?: string
      nodes?: number
      gpusPerNode?: number
      backend?: string
      /** Elastic torchrun job: runs between minNodes and nodes workers; desiredNodes resizes it live. */
      elastic?: { minNodes: number; desiredNodes?: number }
    }
    resources?: {
      requests?: { cpu?: string | number; memory?: string }
      limits?: { cpu?: string | number; memory?: string }
    }
  }
  status?: {
    phase: 'Pending' | 'Running' | 'Completed' | 'Succeeded' | 'Failed' | 'Queued'
    message?: string
    startTime?: string
    completionTime?: string
    elastic?: { currentNodes?: number; desiredNodes?: number; resizes?: number; lastResizeTime?: string }
  }
}

export interface GryviaQuota {
  apiVersion: string
  kind: string
  metadata: {
    name: string
  }
  spec: {
    team: string
    namespaces: string[]
    gpuQuota: {
      maxGPUs: number
      maxGPUsPerJob: number
      allowedGPUTypes: string[]
      maxRunningJobs: number
    }
    budget?: {
      monthlyBudget: number
      alertThreshold: number
      hardLimit: boolean
    }
    priority: number
  }
  status?: {
    phase: string
    currentUsage: {
      allocatedGPUs: number
      runningJobs: number
      queuedJobs: number
      gpuHours: number
    }
    budgetStatus?: {
      spentThisMonth: number
      remainingBudget: number
      percentUsed: number
      projectedSpend: number
    }
  }
}

export interface GryviaGpuNode {
  apiVersion: string
  kind: string
  metadata: {
    name: string
  }
  spec: {
    nodeName: string
    gpuType: string
    gpuCount: number
    memoryGB?: number
    rdma?: boolean
    interconnect?: string
  }
  status?: {
    phase: string
    gpuStatus?: Array<{
      index: number
      uuid?: string
      health?: string
      temperature?: number
      utilization?: number
      memoryUsed?: number
      memoryTotal?: number
    }>
  }
}
