// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SearchAlgorithm defines the hyperparameter search strategy
type SearchAlgorithm string

const (
	SearchAlgorithmGrid     SearchAlgorithm = "grid"
	SearchAlgorithmRandom   SearchAlgorithm = "random"
	SearchAlgorithmBayesian SearchAlgorithm = "bayesian"
	SearchAlgorithmASHA     SearchAlgorithm = "asha"
)

// ObjectiveDirection defines whether to minimize or maximize the objective metric
type ObjectiveDirection string

const (
	ObjectiveMinimize ObjectiveDirection = "minimize"
	ObjectiveMaximize ObjectiveDirection = "maximize"
)

// ParameterType defines the type of a hyperparameter
type ParameterType string

const (
	ParameterTypeFloat       ParameterType = "float"
	ParameterTypeInt         ParameterType = "int"
	ParameterTypeCategorical ParameterType = "categorical"
)

// GryviaAutoTunerSpec defines the desired state of GryviaAutoTuner
type GryviaAutoTunerSpec struct {
	// SearchAlgorithm is the hyperparameter search strategy (grid, random, bayesian, asha)
	SearchAlgorithm SearchAlgorithm `json:"searchAlgorithm"`

	// ParameterSpace defines the hyperparameters and their search ranges
	ParameterSpace []ParameterSpec `json:"parameterSpace"`

	// Objective defines the metric to optimize and its direction
	Objective ObjectiveSpec `json:"objective"`

	// MaxTrials is the maximum number of trials to run
	MaxTrials int32 `json:"maxTrials"`

	// Parallelism is the maximum number of trials to run concurrently
	Parallelism int32 `json:"parallelism,omitempty"`

	// EarlyStoppingRounds stops tuning if no improvement for this many rounds
	EarlyStoppingRounds int32 `json:"earlyStoppingRounds,omitempty"`

	// JobTemplate defines the GryviaAIJob template used for each trial
	JobTemplate GryviaAIJobSpec `json:"jobTemplate"`

	// ASHAConfig configures the ASHA early-stopping scheduler (used when searchAlgorithm is asha)
	ASHAConfig *ASHAConfig `json:"ashaConfig,omitempty"`
}

// ParameterSpec defines a single hyperparameter in the search space
type ParameterSpec struct {
	// Name of the hyperparameter
	Name string `json:"name"`

	// Type of the parameter (float, int, categorical)
	Type ParameterType `json:"type"`

	// Min value for float/int parameters
	Min *float64 `json:"min,omitempty"`

	// Max value for float/int parameters
	Max *float64 `json:"max,omitempty"`

	// Scale for float parameters (linear or log)
	Scale string `json:"scale,omitempty"`

	// Step for int parameters
	Step *int32 `json:"step,omitempty"`

	// Values for categorical parameters or discrete int/float sets
	Values []string `json:"values,omitempty"`
}

// ObjectiveSpec defines the optimization objective
type ObjectiveSpec struct {
	// MetricName is the name of the metric to optimize
	MetricName string `json:"metricName"`

	// Direction is whether to minimize or maximize the metric
	Direction ObjectiveDirection `json:"direction"`
}

// ASHAConfig configures the Asynchronous Successive Halving Algorithm
type ASHAConfig struct {
	// MaxEpochs is the maximum resource (e.g., epochs) per trial
	MaxEpochs int32 `json:"maxEpochs"`

	// ReductionFactor controls how aggressively trials are pruned (default: 3)
	ReductionFactor int32 `json:"reductionFactor,omitempty"`

	// MinResource is the minimum resource to allocate before first pruning decision
	MinResource int32 `json:"minResource,omitempty"`
}

// TrialPhase represents the current state of a trial
type TrialPhase string

const (
	TrialPhasePending   TrialPhase = "Pending"
	TrialPhaseRunning   TrialPhase = "Running"
	TrialPhaseSucceeded TrialPhase = "Succeeded"
	TrialPhaseFailed    TrialPhase = "Failed"
	TrialPhaseStopped   TrialPhase = "Stopped"
)

// TrialResult holds the result of a single trial
type TrialResult struct {
	// Name is the unique name of the trial (typically tuner-name-trial-N)
	Name string `json:"name"`

	// Parameters is a map of hyperparameter name to value used in this trial
	Parameters map[string]string `json:"parameters"`

	// MetricValue is the final value of the objective metric
	MetricValue *float64 `json:"metricValue,omitempty"`

	// IntermediateMetrics tracks metric values at intermediate steps for early stopping
	IntermediateMetrics []IntermediateMetric `json:"intermediateMetrics,omitempty"`

	// Phase is the current phase of the trial
	Phase TrialPhase `json:"phase"`

	// JobName is the name of the GryviaAIJob created for this trial
	JobName string `json:"jobName,omitempty"`

	// StartTime is when the trial started
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the trial finished
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Message provides additional information about the trial status
	Message string `json:"message,omitempty"`
}

// IntermediateMetric records a metric value at a specific step
type IntermediateMetric struct {
	// Step is the training step or epoch
	Step int32 `json:"step"`

	// Value is the metric value at this step
	Value float64 `json:"value"`
}

// GryviaAutoTunerStatus defines the observed state of GryviaAutoTuner
type GryviaAutoTunerStatus struct {
	// Phase is the overall phase (Pending, Running, Succeeded, Failed)
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Trials is the list of all trial results
	Trials []TrialResult `json:"trials,omitempty"`

	// BestTrial is the trial with the best objective metric value
	BestTrial *TrialResult `json:"bestTrial,omitempty"`

	// TrialsCompleted is the number of completed trials
	TrialsCompleted int32 `json:"trialsCompleted,omitempty"`

	// TrialsRunning is the number of currently running trials
	TrialsRunning int32 `json:"trialsRunning,omitempty"`

	// TrialsFailed is the number of failed trials
	TrialsFailed int32 `json:"trialsFailed,omitempty"`

	// StartTime is when the tuning started
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the tuning finished
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Message provides additional information about the current phase
	Message string `json:"message,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced
//+kubebuilder:printcolumn:name="Algorithm",type=string,JSONPath=`.spec.searchAlgorithm`
//+kubebuilder:printcolumn:name="MaxTrials",type=integer,JSONPath=`.spec.maxTrials`
//+kubebuilder:printcolumn:name="Completed",type=integer,JSONPath=`.status.trialsCompleted`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaAutoTuner is the Schema for the gryviaautotuners API
type GryviaAutoTuner struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaAutoTunerSpec   `json:"spec,omitempty"`
	Status GryviaAutoTunerStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaAutoTunerList contains a list of GryviaAutoTuner
type GryviaAutoTunerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaAutoTuner `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaAutoTuner{}, &GryviaAutoTunerList{})
}
