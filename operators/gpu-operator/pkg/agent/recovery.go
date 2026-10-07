package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Actions a request can ask for (annotation AnnAction; empty means reset).
const (
	ActionReset        = "reset"
	ActionDriverReload = "driver-reload"
	ActionReboot       = "reboot"

	StateInProgress = "InProgress"

	RebootKured   = "kured"
	RebootNsenter = "nsenter"

	// LeaseName is the cluster-wide lease that lets one node at a time reload its driver or reboot.
	LeaseName          = "gryvia-gpu-recovery"
	leaseSeconds int32 = 3600

	DriverReloadTimeout = 15 * time.Minute
	RebootTimeout       = 30 * time.Minute
)

// DriverPodApps are the app labels of the NVIDIA GPU Operator pods a driver reload restarts on the node.
var DriverPodApps = []string{"nvidia-driver-daemonset", "nvidia-device-plugin-daemonset", "nvidia-dcgm", "nvidia-dcgm-exporter"}

// Rebooter starts a reboot of this node.
type Rebooter interface {
	Reboot(ctx context.Context, method string) error
}

// HostRebooter writes the kured sentinel (kured drains, takes its lock and reboots) or, with nsenter, runs
// systemctl reboot in the host's namespaces (needs hostPID and a privileged container).
type HostRebooter struct{ Sentinel string }

func (h HostRebooter) Reboot(ctx context.Context, method string) error {
	switch method {
	case RebootKured, "":
		if h.Sentinel == "" {
			return fmt.Errorf("no kured sentinel path configured")
		}
		if err := os.MkdirAll(filepath.Dir(h.Sentinel), 0o755); err != nil {
			return err
		}
		return os.WriteFile(h.Sentinel, []byte("gryvia gpu recovery\n"), 0o644)
	case RebootNsenter:
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--", "systemctl", "reboot").CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return fmt.Errorf("unknown reboot method %q", method)
	}
}

func requestedAction(node *corev1.Node) string {
	a := strings.TrimSpace(node.Annotations[AnnAction])
	if a == "" {
		return ActionReset
	}
	return a
}

// decideRecovery handles driver-reload and reboot requests once the node is cordoned and free of GPU pods.
func (a *Agent) decideRecovery(ctx context.Context, node *corev1.Node, action string, mk func(state, msg string) Result) Result {
	switch {
	case action == ActionDriverReload && !a.AllowDriverReload:
		return mk(StateInvalid, "driver reload is not allowed on this agent (resetAgent.allowDriverReload)")
	case action == ActionReboot && !a.AllowReboot:
		return mk(StateInvalid, "reboot is not allowed on this agent (resetAgent.allowReboot)")
	}
	if !a.Execute {
		if action == ActionReboot {
			return mk(StateDryRun, "dry-run: would reboot the node ("+a.rebootMethod()+")")
		}
		return mk(StateDryRun, "dry-run: would restart the GPU Operator driver, device plugin and DCGM pods on the node")
	}
	if holder, err := a.acquireLease(ctx); err != nil {
		return mk(StateBlocked, "cannot take the recovery lease: "+err.Error())
	} else if holder != "" {
		return mk(StateBlocked, "node "+holder+" is reloading its driver or rebooting; one node at a time")
	}
	started := a.now().UTC().Format(time.RFC3339)
	if action == ActionReboot {
		res := mk(StateInProgress, "reboot requested ("+a.rebootMethod()+"); waiting for the node to come back")
		res.Action, res.Started, res.BootID = action, started, node.Status.NodeInfo.BootID
		if err := a.rebooter().Reboot(ctx, a.rebootMethod()); err != nil {
			a.releaseLease(ctx)
			return mk(StateFailed, truncate("reboot failed to start: "+err.Error(), 500))
		}
		return res
	}
	apps, err := a.deleteDriverPods(ctx)
	if err != nil {
		a.releaseLease(ctx)
		return mk(StateFailed, truncate("driver reload failed: "+err.Error(), 500))
	}
	if len(apps) == 0 {
		a.releaseLease(ctx)
		return mk(StateFailed, "driver reload: no GPU Operator driver, device plugin or DCGM pods on the node")
	}
	res := mk(StateInProgress, "restarted "+strings.Join(apps, ", ")+"; waiting for them to be ready again")
	res.Action, res.Started, res.Apps = action, started, apps
	return res
}

// progress follows an InProgress driver reload or reboot to Done or Failed.
func (a *Agent) progress(ctx context.Context, node *corev1.Node, prev Result) Result {
	mk := func(state, msg string) Result {
		r := prev
		r.State, r.Message, r.At = state, msg, a.now().UTC().Format(time.RFC3339)
		return r
	}
	started, err := time.Parse(time.RFC3339, prev.Started)
	if err != nil {
		a.releaseLease(ctx)
		return mk(StateFailed, "in-progress record has no valid start time")
	}
	_, _ = a.acquireLease(ctx) // renew while the action runs
	switch prev.Action {
	case ActionReboot:
		if node.Status.NodeInfo.BootID != "" && node.Status.NodeInfo.BootID != prev.BootID && nodeReady(node) {
			a.releaseLease(ctx)
			return mk(StateDone, "node rebooted (boot id changed) and is Ready")
		}
		if a.now().Sub(started) > RebootTimeout {
			a.releaseLease(ctx)
			return mk(StateFailed, fmt.Sprintf("node did not come back within %s", RebootTimeout))
		}
		return prev
	case ActionDriverReload:
		waiting, err := a.driverPodsNotReady(ctx, prev.Apps, started)
		if err != nil {
			return prev
		}
		if len(waiting) == 0 {
			a.releaseLease(ctx)
			return mk(StateDone, "driver reload completed: "+strings.Join(prev.Apps, ", ")+" ready again")
		}
		if a.now().Sub(started) > DriverReloadTimeout {
			a.releaseLease(ctx)
			return mk(StateFailed, fmt.Sprintf("not ready within %s: %s", DriverReloadTimeout, strings.Join(waiting, ", ")))
		}
		return prev
	default:
		a.releaseLease(ctx)
		return mk(StateFailed, "unknown in-progress action "+prev.Action)
	}
}

func nodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (a *Agent) rebootMethod() string {
	if a.RebootMethod == "" {
		return RebootKured
	}
	return a.RebootMethod
}

func (a *Agent) rebooter() Rebooter {
	if a.Rebooter != nil {
		return a.Rebooter
	}
	return HostRebooter{Sentinel: a.RebootSentinel}
}

func (a *Agent) podsOnNode(ctx context.Context) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := a.Client.List(ctx, pods, client.MatchingFields{"spec.nodeName": a.Node}); err != nil {
		pods = &corev1.PodList{}
		if err := a.Client.List(ctx, pods); err != nil {
			return nil, err
		}
	}
	var out []corev1.Pod
	for _, p := range pods.Items {
		if p.Spec.NodeName == a.Node {
			out = append(out, p)
		}
	}
	return out, nil
}

// deleteDriverPods deletes this node's GPU Operator driver, device plugin and DCGM pods; their DaemonSets
// recreate them, which reloads the driver. It returns the apps that had a pod.
func (a *Agent) deleteDriverPods(ctx context.Context) ([]string, error) {
	pods, err := a.podsOnNode(ctx)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, app := range DriverPodApps {
		want[app] = true
	}
	seen := map[string]bool{}
	for i := range pods {
		p := &pods[i]
		app := p.Labels["app"]
		if !want[app] || p.DeletionTimestamp != nil {
			continue
		}
		if err := a.Client.Delete(ctx, p); err != nil && !errors.IsNotFound(err) {
			return nil, fmt.Errorf("delete %s/%s: %w", p.Namespace, p.Name, err)
		}
		seen[app] = true
	}
	var apps []string
	for app := range seen {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	return apps, nil
}

// driverPodsNotReady lists the apps that have no Ready pod created after the reload started.
func (a *Agent) driverPodsNotReady(ctx context.Context, apps []string, started time.Time) ([]string, error) {
	pods, err := a.podsOnNode(ctx)
	if err != nil {
		return nil, err
	}
	ready := map[string]bool{}
	for _, p := range pods {
		if p.DeletionTimestamp != nil || p.CreationTimestamp.Time.Before(started.Add(-time.Second)) {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready[p.Labels["app"]] = true
			}
		}
	}
	var waiting []string
	for _, app := range apps {
		if !ready[app] {
			waiting = append(waiting, app)
		}
	}
	return waiting, nil
}

// acquireLease takes or renews the cluster-wide recovery lease for this node. It returns the other holder's name
// when another node holds an unexpired lease. Without a lease namespace there is no coordination.
func (a *Agent) acquireLease(ctx context.Context) (string, error) {
	if a.LeaseNamespace == "" {
		return "", nil
	}
	now := metav1.NewMicroTime(a.now())
	dur := leaseSeconds
	me := a.Node
	lease := &coordinationv1.Lease{}
	err := a.Client.Get(ctx, types.NamespacedName{Namespace: a.LeaseNamespace, Name: LeaseName}, lease)
	if errors.IsNotFound(err) {
		lease = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: LeaseName, Namespace: a.LeaseNamespace},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &me, LeaseDurationSeconds: &dur, AcquireTime: &now, RenewTime: &now},
		}
		return "", a.Client.Create(ctx, lease)
	}
	if err != nil {
		return "", err
	}
	holder := ""
	if lease.Spec.HolderIdentity != nil {
		holder = *lease.Spec.HolderIdentity
	}
	if holder != "" && holder != me && lease.Spec.RenewTime != nil && lease.Spec.LeaseDurationSeconds != nil &&
		a.now().Before(lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds)*time.Second)) {
		return holder, nil
	}
	if holder != me {
		lease.Spec.AcquireTime = &now
	}
	lease.Spec.HolderIdentity, lease.Spec.LeaseDurationSeconds, lease.Spec.RenewTime = &me, &dur, &now
	return "", a.Client.Update(ctx, lease)
}

func (a *Agent) releaseLease(ctx context.Context) {
	if a.LeaseNamespace == "" {
		return
	}
	lease := &coordinationv1.Lease{}
	if err := a.Client.Get(ctx, types.NamespacedName{Namespace: a.LeaseNamespace, Name: LeaseName}, lease); err != nil {
		return
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != a.Node {
		return
	}
	lease.Spec.HolderIdentity = nil
	_ = a.Client.Update(ctx, lease)
}
