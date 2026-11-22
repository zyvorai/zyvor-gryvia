// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package gryvia provides a typed Go client for Gryvia resources.
//
// This package re-exports all API types from the ai-operator for convenient
// use by external consumers without directly depending on the operator module.
package gryvia

import (
	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// --- GryviaAIJob re-exports ---

type GryviaAIJob = gryviav1.GryviaAIJob
type GryviaAIJobList = gryviav1.GryviaAIJobList
type GryviaAIJobSpec = gryviav1.GryviaAIJobSpec
type GryviaAIJobStatus = gryviav1.GryviaAIJobStatus
type DistributedConfig = gryviav1.DistributedConfig
type JobMetrics = gryviav1.JobMetrics

// --- GryviaAutoTuner re-exports ---

type GryviaAutoTuner = gryviav1.GryviaAutoTuner
type GryviaAutoTunerList = gryviav1.GryviaAutoTunerList
type GryviaAutoTunerSpec = gryviav1.GryviaAutoTunerSpec
type GryviaAutoTunerStatus = gryviav1.GryviaAutoTunerStatus
type ParameterSpec = gryviav1.ParameterSpec
type ObjectiveSpec = gryviav1.ObjectiveSpec
type ASHAConfig = gryviav1.ASHAConfig
type TrialResult = gryviav1.TrialResult
type IntermediateMetric = gryviav1.IntermediateMetric

// Search algorithm constants
const (
	SearchAlgorithmGrid     = gryviav1.SearchAlgorithmGrid
	SearchAlgorithmRandom   = gryviav1.SearchAlgorithmRandom
	SearchAlgorithmBayesian = gryviav1.SearchAlgorithmBayesian
	SearchAlgorithmASHA     = gryviav1.SearchAlgorithmASHA
)

// Objective direction constants
const (
	ObjectiveMinimize = gryviav1.ObjectiveMinimize
	ObjectiveMaximize = gryviav1.ObjectiveMaximize
)

// Parameter type constants
const (
	ParameterTypeFloat       = gryviav1.ParameterTypeFloat
	ParameterTypeInt         = gryviav1.ParameterTypeInt
	ParameterTypeCategorical = gryviav1.ParameterTypeCategorical
)

// --- GryviaWorkflow re-exports ---

type GryviaWorkflow = gryviav1.GryviaWorkflow
type GryviaWorkflowList = gryviav1.GryviaWorkflowList
type GryviaWorkflowSpec = gryviav1.GryviaWorkflowSpec
type GryviaWorkflowStatus = gryviav1.GryviaWorkflowStatus
type WorkflowStep = gryviav1.WorkflowStep
type ScriptStep = gryviav1.ScriptStep
type WebhookStep = gryviav1.WebhookStep
type StepStatus = gryviav1.StepStatus

// Step type constants
const (
	StepTypeJob     = gryviav1.StepTypeJob
	StepTypeScript  = gryviav1.StepTypeScript
	StepTypeWebhook = gryviav1.StepTypeWebhook
)

// --- GryviaModelRegistry re-exports ---

type GryviaModelRegistry = gryviav1.GryviaModelRegistry
type GryviaModelRegistryList = gryviav1.GryviaModelRegistryList
type GryviaModelRegistrySpec = gryviav1.GryviaModelRegistrySpec
type GryviaModelRegistryStatus = gryviav1.GryviaModelRegistryStatus
type ModelSource = gryviav1.ModelSource
type ModelArtifacts = gryviav1.ModelArtifacts
type ServingConfig = gryviav1.ServingConfig

// Model stage constants
const (
	ModelStageDev        = gryviav1.ModelStageDev
	ModelStageStaging    = gryviav1.ModelStageStaging
	ModelStageProduction = gryviav1.ModelStageProduction
	ModelStageArchived   = gryviav1.ModelStageArchived
)

// --- GryviaInferenceService re-exports ---

type GryviaInferenceService = gryviav1.GryviaInferenceService
type GryviaInferenceServiceList = gryviav1.GryviaInferenceServiceList
type GryviaInferenceServiceSpec = gryviav1.GryviaInferenceServiceSpec
type GryviaInferenceServiceStatus = gryviav1.GryviaInferenceServiceStatus
type AutoscalingConfig = gryviav1.AutoscalingConfig
type CanaryConfig = gryviav1.CanaryConfig
type HealthCheckConfig = gryviav1.HealthCheckConfig
type CanaryStatus = gryviav1.CanaryStatus

// Inference backend constants
const (
	BackendTriton      = gryviav1.BackendTriton
	BackendVLLM        = gryviav1.BackendVLLM
	BackendTensorRTLLM = gryviav1.BackendTensorRTLLM
	BackendTorchServe  = gryviav1.BackendTorchServe
)

// --- GryviaWorkspace re-exports ---

type GryviaWorkspace = gryviav1.GryviaWorkspace
type GryviaWorkspaceList = gryviav1.GryviaWorkspaceList
type GryviaWorkspaceSpec = gryviav1.GryviaWorkspaceSpec
type GryviaWorkspaceStatus = gryviav1.GryviaWorkspaceStatus

// Workspace type constants
const (
	WorkspaceTypeJupyter = gryviav1.WorkspaceTypeJupyter
	WorkspaceTypeVSCode  = gryviav1.WorkspaceTypeVSCode
)

// --- GryviaGPUNode re-exports ---

type GryviaGPUNode = gryviav1.GryviaGpuNode
type GryviaGPUNodeList = gryviav1.GryviaGpuNodeList

// --- GryviaCheckpointGuard re-exports ---

type GryviaCheckpointGuard = gryviav1.GryviaCheckpointGuard
type GryviaCheckpointGuardList = gryviav1.GryviaCheckpointGuardList

// --- GroupVersion ---

var GroupVersion = gryviav1.GroupVersion
var AddToScheme = gryviav1.AddToScheme
