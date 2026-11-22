// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package gryvia

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// GryviaClient provides typed methods for managing Gryvia resources.
type GryviaClient struct {
	client client.Client
}

// NewClient creates a new GryviaClient using the provided Kubernetes REST config.
func NewClient(cfg *rest.Config) (*GryviaClient, error) {
	scheme := runtime.NewScheme()
	if err := gryviav1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add Gryvia types to scheme: %w", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	return &GryviaClient{client: c}, nil
}

// NewClientFromExisting wraps an existing controller-runtime client.
// The caller must ensure that Gryvia types are registered in the client's scheme.
func NewClientFromExisting(c client.Client) *GryviaClient {
	return &GryviaClient{client: c}
}

// --- GryviaAIJob operations ---

// CreateJob creates a new GryviaAIJob in the given namespace.
func (t *GryviaClient) CreateJob(ctx context.Context, job *GryviaAIJob) error {
	return t.client.Create(ctx, job)
}

// GetJob retrieves a GryviaAIJob by name and namespace.
func (t *GryviaClient) GetJob(ctx context.Context, namespace, name string) (*GryviaAIJob, error) {
	job := &GryviaAIJob{}
	err := t.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, job)
	if err != nil {
		return nil, err
	}
	return job, nil
}

// ListJobs returns all GryviaAIJobs in the given namespace.
func (t *GryviaClient) ListJobs(ctx context.Context, namespace string, opts ...client.ListOption) (*GryviaAIJobList, error) {
	list := &GryviaAIJobList{}
	allOpts := append([]client.ListOption{client.InNamespace(namespace)}, opts...)
	err := t.client.List(ctx, list, allOpts...)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// DeleteJob deletes a GryviaAIJob by name and namespace.
func (t *GryviaClient) DeleteJob(ctx context.Context, namespace, name string) error {
	job := &GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return t.client.Delete(ctx, job)
}

// UpdateJobStatus updates the status of a GryviaAIJob.
func (t *GryviaClient) UpdateJobStatus(ctx context.Context, job *GryviaAIJob) error {
	return t.client.Status().Update(ctx, job)
}

// WatchJob returns a watch.Interface that receives events for GryviaAIJob changes.
// Note: This requires the client to support watching (e.g., a cached client from a Manager).
// For non-cached clients, consider using an informer instead.
func (t *GryviaClient) WatchJob(ctx context.Context, namespace string) (watch.Interface, error) {
	// controller-runtime's client.Client does not natively expose Watch.
	// Callers using a manager should use the informer cache.
	// This method documents the intended interface; a full implementation
	// would use client-go's typed client or informer factories.
	return nil, fmt.Errorf("Watch requires an informer-backed client; use manager's cache instead")
}

// --- GryviaAutoTuner operations ---

// CreateAutoTuner creates a new GryviaAutoTuner.
func (t *GryviaClient) CreateAutoTuner(ctx context.Context, tuner *GryviaAutoTuner) error {
	return t.client.Create(ctx, tuner)
}

// GetAutoTuner retrieves a GryviaAutoTuner by name and namespace.
func (t *GryviaClient) GetAutoTuner(ctx context.Context, namespace, name string) (*GryviaAutoTuner, error) {
	tuner := &GryviaAutoTuner{}
	err := t.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, tuner)
	if err != nil {
		return nil, err
	}
	return tuner, nil
}

// ListAutoTuners returns all GryviaAutoTuners in the given namespace.
func (t *GryviaClient) ListAutoTuners(ctx context.Context, namespace string, opts ...client.ListOption) (*GryviaAutoTunerList, error) {
	list := &GryviaAutoTunerList{}
	allOpts := append([]client.ListOption{client.InNamespace(namespace)}, opts...)
	err := t.client.List(ctx, list, allOpts...)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// DeleteAutoTuner deletes a GryviaAutoTuner by name and namespace.
func (t *GryviaClient) DeleteAutoTuner(ctx context.Context, namespace, name string) error {
	tuner := &GryviaAutoTuner{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return t.client.Delete(ctx, tuner)
}

// --- GryviaWorkflow operations ---

// CreateWorkflow creates a new GryviaWorkflow.
func (t *GryviaClient) CreateWorkflow(ctx context.Context, wf *GryviaWorkflow) error {
	return t.client.Create(ctx, wf)
}

// GetWorkflow retrieves a GryviaWorkflow by name and namespace.
func (t *GryviaClient) GetWorkflow(ctx context.Context, namespace, name string) (*GryviaWorkflow, error) {
	wf := &GryviaWorkflow{}
	err := t.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, wf)
	if err != nil {
		return nil, err
	}
	return wf, nil
}

// ListWorkflows returns all GryviaWorkflows in the given namespace.
func (t *GryviaClient) ListWorkflows(ctx context.Context, namespace string, opts ...client.ListOption) (*GryviaWorkflowList, error) {
	list := &GryviaWorkflowList{}
	allOpts := append([]client.ListOption{client.InNamespace(namespace)}, opts...)
	err := t.client.List(ctx, list, allOpts...)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// DeleteWorkflow deletes a GryviaWorkflow by name and namespace.
func (t *GryviaClient) DeleteWorkflow(ctx context.Context, namespace, name string) error {
	wf := &GryviaWorkflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return t.client.Delete(ctx, wf)
}

// --- GryviaModelRegistry operations ---

// CreateModelRegistry creates a new GryviaModelRegistry entry.
func (t *GryviaClient) CreateModelRegistry(ctx context.Context, model *GryviaModelRegistry) error {
	return t.client.Create(ctx, model)
}

// GetModelRegistry retrieves a GryviaModelRegistry by name and namespace.
func (t *GryviaClient) GetModelRegistry(ctx context.Context, namespace, name string) (*GryviaModelRegistry, error) {
	model := &GryviaModelRegistry{}
	err := t.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, model)
	if err != nil {
		return nil, err
	}
	return model, nil
}

// ListModelRegistries returns all GryviaModelRegistry entries in the given namespace.
func (t *GryviaClient) ListModelRegistries(ctx context.Context, namespace string, opts ...client.ListOption) (*GryviaModelRegistryList, error) {
	list := &GryviaModelRegistryList{}
	allOpts := append([]client.ListOption{client.InNamespace(namespace)}, opts...)
	err := t.client.List(ctx, list, allOpts...)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// DeleteModelRegistry deletes a GryviaModelRegistry by name and namespace.
func (t *GryviaClient) DeleteModelRegistry(ctx context.Context, namespace, name string) error {
	model := &GryviaModelRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return t.client.Delete(ctx, model)
}

// PromoteModel updates the stage of a model registry entry.
func (t *GryviaClient) PromoteModel(ctx context.Context, namespace, name string, stage gryviav1.ModelStage) error {
	model, err := t.GetModelRegistry(ctx, namespace, name)
	if err != nil {
		return err
	}
	model.Spec.Stage = stage
	return t.client.Update(ctx, model)
}

// --- GryviaInferenceService operations ---

// CreateInferenceService creates a new GryviaInferenceService.
func (t *GryviaClient) CreateInferenceService(ctx context.Context, svc *GryviaInferenceService) error {
	return t.client.Create(ctx, svc)
}

// GetInferenceService retrieves a GryviaInferenceService by name and namespace.
func (t *GryviaClient) GetInferenceService(ctx context.Context, namespace, name string) (*GryviaInferenceService, error) {
	svc := &GryviaInferenceService{}
	err := t.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, svc)
	if err != nil {
		return nil, err
	}
	return svc, nil
}

// ListInferenceServices returns all GryviaInferenceServices in the given namespace.
func (t *GryviaClient) ListInferenceServices(ctx context.Context, namespace string, opts ...client.ListOption) (*GryviaInferenceServiceList, error) {
	list := &GryviaInferenceServiceList{}
	allOpts := append([]client.ListOption{client.InNamespace(namespace)}, opts...)
	err := t.client.List(ctx, list, allOpts...)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// DeleteInferenceService deletes a GryviaInferenceService by name and namespace.
func (t *GryviaClient) DeleteInferenceService(ctx context.Context, namespace, name string) error {
	svc := &GryviaInferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return t.client.Delete(ctx, svc)
}

// --- GryviaWorkspace operations ---

// CreateWorkspace creates a new GryviaWorkspace.
func (t *GryviaClient) CreateWorkspace(ctx context.Context, ws *GryviaWorkspace) error {
	return t.client.Create(ctx, ws)
}

// GetWorkspace retrieves a GryviaWorkspace by name and namespace.
func (t *GryviaClient) GetWorkspace(ctx context.Context, namespace, name string) (*GryviaWorkspace, error) {
	ws := &GryviaWorkspace{}
	err := t.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, ws)
	if err != nil {
		return nil, err
	}
	return ws, nil
}

// ListWorkspaces returns all GryviaWorkspaces in the given namespace.
func (t *GryviaClient) ListWorkspaces(ctx context.Context, namespace string, opts ...client.ListOption) (*GryviaWorkspaceList, error) {
	list := &GryviaWorkspaceList{}
	allOpts := append([]client.ListOption{client.InNamespace(namespace)}, opts...)
	err := t.client.List(ctx, list, allOpts...)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// DeleteWorkspace deletes a GryviaWorkspace by name and namespace.
func (t *GryviaClient) DeleteWorkspace(ctx context.Context, namespace, name string) error {
	ws := &GryviaWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return t.client.Delete(ctx, ws)
}

// PauseWorkspace sets the workspace to paused state, freeing GPU resources while retaining data.
func (t *GryviaClient) PauseWorkspace(ctx context.Context, namespace, name string) error {
	ws, err := t.GetWorkspace(ctx, namespace, name)
	if err != nil {
		return err
	}
	ws.Spec.Paused = true
	return t.client.Update(ctx, ws)
}

// ResumeWorkspace resumes a paused workspace, recreating the pod with the existing PVC.
func (t *GryviaClient) ResumeWorkspace(ctx context.Context, namespace, name string) error {
	ws, err := t.GetWorkspace(ctx, namespace, name)
	if err != nil {
		return err
	}
	ws.Spec.Paused = false
	return t.client.Update(ctx, ws)
}

// --- GryviaGPUNode operations ---

// GetGPUNode retrieves a GryviaGPUNode by name.
func (t *GryviaClient) GetGPUNode(ctx context.Context, name string) (*GryviaGPUNode, error) {
	node := &GryviaGPUNode{}
	err := t.client.Get(ctx, types.NamespacedName{Name: name}, node)
	if err != nil {
		return nil, err
	}
	return node, nil
}

// ListGPUNodes returns all GryviaGPUNodes.
func (t *GryviaClient) ListGPUNodes(ctx context.Context, opts ...client.ListOption) (*GryviaGPUNodeList, error) {
	list := &GryviaGPUNodeList{}
	err := t.client.List(ctx, list, opts...)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// --- GryviaCheckpointGuard operations ---

// CreateCheckpointGuard creates a new GryviaCheckpointGuard.
func (t *GryviaClient) CreateCheckpointGuard(ctx context.Context, guard *GryviaCheckpointGuard) error {
	return t.client.Create(ctx, guard)
}

// GetCheckpointGuard retrieves a GryviaCheckpointGuard by name and namespace.
func (t *GryviaClient) GetCheckpointGuard(ctx context.Context, namespace, name string) (*GryviaCheckpointGuard, error) {
	guard := &GryviaCheckpointGuard{}
	err := t.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, guard)
	if err != nil {
		return nil, err
	}
	return guard, nil
}

// ListCheckpointGuards returns all GryviaCheckpointGuards in the given namespace.
func (t *GryviaClient) ListCheckpointGuards(ctx context.Context, namespace string, opts ...client.ListOption) (*GryviaCheckpointGuardList, error) {
	list := &GryviaCheckpointGuardList{}
	allOpts := append([]client.ListOption{client.InNamespace(namespace)}, opts...)
	err := t.client.List(ctx, list, allOpts...)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// DeleteCheckpointGuard deletes a GryviaCheckpointGuard by name and namespace.
func (t *GryviaClient) DeleteCheckpointGuard(ctx context.Context, namespace, name string) error {
	guard := &GryviaCheckpointGuard{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return t.client.Delete(ctx, guard)
}
