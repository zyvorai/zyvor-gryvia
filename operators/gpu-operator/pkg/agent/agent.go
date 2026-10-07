// Package agent is the node-local GPU reset agent.
//
// A person (or later, a controller) asks for a reset by annotating a Node:
//
//	gryvia.io/gpu-reset-request: "<id>"        any non-empty id up to 63 characters; a new id is a new request
//	gryvia.io/gpu-reset-gpus:    "0,1"          optional GPU indices; empty = all GPUs
//
// The agent on that node acts only when ALL of these hold: the node is cordoned (spec.unschedulable), no
// non-terminated pod other than DaemonSet and static pods requests nvidia.com/gpu, and the request id differs
// from gryvia.io/gpu-reset-observed. It then runs `nvidia-smi --gpu-reset -i <indices>` (or, in dry-run, only
// says what it would run) and records the outcome in gryvia.io/gpu-reset-result. It never cordons, drains,
// uncordons or removes taints; the quarantine and drain stay with the health check and the operator.
//
// DEFAULT IS DRY-RUN. Real execution needs the Execute option, an nvidia-smi the agent can run, and enough
// privilege on the node; none of that has been verified on real hardware.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	AnnRequest  = "gryvia.io/gpu-reset-request"
	AnnGPUs     = "gryvia.io/gpu-reset-gpus"
	AnnObserved = "gryvia.io/gpu-reset-observed"
	AnnResult   = "gryvia.io/gpu-reset-result"
	// AnnAction picks reset (default), driver-reload or reboot.
	AnnAction = "gryvia.io/gpu-reset-action"

	// ResetTimeout bounds one nvidia-smi invocation.
	ResetTimeout = 120 * time.Second
	maxGPUIndex  = 63
)

// Result states.
const (
	StateDryRun  = "DryRun"
	StateDone    = "Done"
	StateFailed  = "Failed"
	StateBlocked = "Blocked"
	StateInvalid = "Invalid"
)

// Result is the JSON stored in AnnResult.
type Result struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
	At      string `json:"at"`
	// Action, Started, BootID and Apps track an InProgress driver reload or reboot.
	Action  string   `json:"action,omitempty"`
	Started string   `json:"started,omitempty"`
	BootID  string   `json:"bootID,omitempty"`
	Apps    []string `json:"apps,omitempty"`
}

// Runner runs the reset command; tests replace it.
type Runner interface {
	Reset(ctx context.Context, gpus []int) (output string, err error)
}

// SMIRunner runs nvidia-smi --gpu-reset.
type SMIRunner struct{ Path string }

func (r SMIRunner) Reset(ctx context.Context, gpus []int) (string, error) {
	args := []string{"--gpu-reset"}
	if len(gpus) > 0 {
		args = append(args, "-i", joinInts(gpus))
	}
	ctx, cancel := context.WithTimeout(ctx, ResetTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.Path, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Agent resets GPUs on one node.
type Agent struct {
	Client  client.Client
	Node    string
	Execute bool   // false = dry-run
	Runner  Runner // nil = SMIRunner{"nvidia-smi"}
	Now     func() time.Time

	// AllowDriverReload and AllowReboot permit the driver-reload and reboot actions (both off by default).
	AllowDriverReload bool
	AllowReboot       bool
	// RebootMethod is kured (default: write RebootSentinel and let kured reboot) or nsenter.
	RebootMethod   string
	RebootSentinel string
	Rebooter       Rebooter // nil = HostRebooter
	// LeaseNamespace holds the one-node-at-a-time lease for driver reloads and reboots ("" = no lease).
	LeaseNamespace string
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func joinInts(v []int) string {
	s := make([]string, len(v))
	for i, n := range v {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ",")
}

// ParseGPUs parses "0,1,3" into sorted unique indices; "" means all (nil). Rejects anything else.
func ParseGPUs(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	seen := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 || n > maxGPUIndex {
			return nil, fmt.Errorf("invalid GPU index %q (want integers 0-%d separated by commas)", part, maxGPUIndex)
		}
		seen[n] = true
	}
	out := make([]int, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	return out, nil
}

func validID(id string) bool {
	if id == "" || len(id) > 63 {
		return false
	}
	for _, c := range id {
		if !(c == '-' || c == '_' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// gpuPodsOn lists the pods that still hold a GPU on the node (DaemonSet and static pods are ignored).
func (a *Agent) gpuPodsOn(ctx context.Context) ([]string, error) {
	pods := &corev1.PodList{}
	if err := a.Client.List(ctx, pods, client.MatchingFields{"spec.nodeName": a.Node}); err != nil {
		// no field index (fake client or uncached): fall back to a full list and filter
		pods = &corev1.PodList{}
		if err := a.Client.List(ctx, pods); err != nil {
			return nil, err
		}
	}
	var busy []string
	for _, p := range pods.Items {
		if p.Spec.NodeName != a.Node || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if o := ownerKind(&p); o == "DaemonSet" || p.Annotations[corev1.MirrorPodAnnotationKey] != "" {
			continue
		}
		for _, c := range p.Spec.Containers {
			if q, ok := c.Resources.Requests["nvidia.com/gpu"]; ok && !q.IsZero() {
				busy = append(busy, p.Namespace+"/"+p.Name)
				break
			}
			if q, ok := c.Resources.Limits["nvidia.com/gpu"]; ok && !q.IsZero() {
				busy = append(busy, p.Namespace+"/"+p.Name)
				break
			}
		}
	}
	sort.Strings(busy)
	return busy, nil
}

func ownerKind(p *corev1.Pod) string {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller {
			return o.Kind
		}
	}
	return ""
}

// Reconcile handles at most one pending request on the node and returns what it recorded ("" when there was
// nothing to do or the request was already handled with the same outcome).
func (a *Agent) Reconcile(ctx context.Context) (string, error) {
	node := &corev1.Node{}
	if err := a.Client.Get(ctx, types.NamespacedName{Name: a.Node}, node); err != nil {
		return "", err
	}
	id := node.Annotations[AnnRequest]
	if id == "" || id == node.Annotations[AnnObserved] {
		return "", nil
	}
	var prevRes Result
	_ = json.Unmarshal([]byte(node.Annotations[AnnResult]), &prevRes)
	var res Result
	if prevRes.ID == id && prevRes.State == StateInProgress {
		res = a.progress(ctx, node, prevRes)
	} else {
		res = a.decide(ctx, node, id)
	}
	if prevRes.ID == id && prevRes.State == res.State && prevRes.Message == res.Message && (res.State == StateBlocked || res.State == StateInProgress) {
		return "", nil // unchanged: do not rewrite the annotation every poll
	}
	body, _ := json.Marshal(res)
	base := node.DeepCopy()
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[AnnResult] = string(body)
	if res.State != StateBlocked && res.State != StateInProgress { // terminal: this request id is done, whatever happened
		node.Annotations[AnnObserved] = id
	}
	if err := a.Client.Patch(ctx, node, client.MergeFrom(base)); err != nil {
		return "", err
	}
	return res.State, nil
}

func (a *Agent) decide(ctx context.Context, node *corev1.Node, id string) Result {
	mk := func(state, msg string) Result {
		return Result{ID: id, State: state, Message: msg, At: a.now().UTC().Format(time.RFC3339)}
	}
	if !validID(id) {
		return mk(StateInvalid, "request id must be 1-63 characters of letters, digits, '-', '_' or '.'")
	}
	gpus, err := ParseGPUs(node.Annotations[AnnGPUs])
	if err != nil {
		return mk(StateInvalid, err.Error())
	}
	if !node.Spec.Unschedulable {
		return mk(StateBlocked, "node is not cordoned; cordon and drain it first")
	}
	busy, err := a.gpuPodsOn(ctx)
	if err != nil {
		return mk(StateBlocked, "cannot list pods: "+err.Error())
	}
	if len(busy) > 0 {
		if len(busy) > 3 {
			busy = append(busy[:3], fmt.Sprintf("and %d more", len(busy)-3))
		}
		return mk(StateBlocked, "GPU pods still on the node: "+strings.Join(busy, ", "))
	}
	switch action := requestedAction(node); action {
	case ActionReset:
	case ActionDriverReload, ActionReboot:
		return a.decideRecovery(ctx, node, action, mk)
	default:
		return mk(StateInvalid, fmt.Sprintf("unknown action %q (reset, driver-reload or reboot)", action))
	}
	target := "all GPUs"
	if len(gpus) > 0 {
		target = "GPU " + joinInts(gpus)
	}
	if !a.Execute {
		return mk(StateDryRun, "dry-run: would run nvidia-smi --gpu-reset for "+target)
	}
	runner := a.Runner
	if runner == nil {
		runner = SMIRunner{Path: "nvidia-smi"}
	}
	out, err := runner.Reset(ctx, gpus)
	if err != nil {
		return mk(StateFailed, truncate(fmt.Sprintf("reset of %s failed: %v: %s", target, err, out), 500))
	}
	return mk(StateDone, truncate("reset of "+target+" completed: "+out, 500))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Run polls until ctx is done. A transient error is returned to onErr and polling continues.
func (a *Agent) Run(ctx context.Context, every time.Duration, onResult func(state string), onErr func(error)) error {
	if a.Node == "" {
		return errors.New("node name is required")
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		state, err := a.Reconcile(ctx)
		if err != nil && onErr != nil {
			onErr(err)
		} else if state != "" && onResult != nil {
			onResult(state)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
