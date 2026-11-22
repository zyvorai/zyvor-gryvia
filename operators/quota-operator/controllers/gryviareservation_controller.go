// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/cron"
)

const (
	reservationFinalizer = "gryvia.io/reservation-finalizer"

	reservationStatePending   = "pending"
	reservationStateActive    = "active"
	reservationStateExpired   = "expired"
	reservationStateCancelled = "cancelled"

	// ReservedTaintKey is the NoSchedule taint on reserved nodes; its value is
	// ReservationOwnerValue(owner). Only pods that tolerate it can land there.
	ReservedTaintKey = "gryvia.io/reserved"
	// LabelReservedFor holds the reservation name on a reserved node. It is the claim: a node
	// with this label belongs to that reservation. Jobs select on it.
	LabelReservedFor = "gryvia.io/reserved-for"
	// LabelReservedBy holds the owner value (same as the taint value).
	LabelReservedBy = "gryvia.io/reserved-by"
	// LabelExclusive mirrors spec.guarantees.exclusive (informational: every reservation is
	// enforced by the taint).
	LabelExclusive = "gryvia.io/exclusive"

	// LabelGPUCount is the fallback per-node GPU count when the node does not advertise
	// nvidia.com/gpu (for example a node labelled by hand).
	LabelGPUCount = "gryvia.io/gpu-count"
	// LabelGPUType is the node label carrying the GPU model.
	LabelGPUType = "gryvia.io/gpu"

	// annotationReservation (or a label of the same key) on a GryviaAIJob names the reservation it uses.
	annotationReservation = "gryvia.io/reservation"

	reservationHeartbeat = time.Minute
)

// GPUResource is the extended resource the NVIDIA device plugin advertises.
var GPUResource = corev1.ResourceName("nvidia.com/gpu")

var errClaimedByOther = errors.New("node is reserved by another reservation")

// GryviaReservationReconciler makes reservations real. While a reservation's window is
// active it claims Ready nodes matching the requested GPU type and count and enforces the
// claim with a NoSchedule taint (gryvia.io/reserved=<owner>) plus labels; the AI operator
// gives jobs that carry the annotation gryvia.io/reservation a matching toleration and
// nodeSelector. Outside the window, on expiry, on deletion and for orphaned nodes the taint
// and labels are removed. Deleting a reservation is how it is cancelled.
//
// Never run against a real cluster yet: covered by fake-client tests and the kind workflow
// .github/workflows/e2e-gpuaas.yml.
type GryviaReservationReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Now returns the current time; tests override it. Defaults to time.Now.
	Now func() time.Time
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviareservations,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviareservations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviareservations/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch

func (r *GryviaReservationReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

var labelValueInvalid = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// ReservationOwnerValue turns the owner name into a valid taint/label value (at most 63
// characters, alphanumeric at both ends). The AI operator computes the same value to build
// the toleration; keep operators/ai-operator/pkg/reservation in step.
func ReservationOwnerValue(name string) string {
	v := labelValueInvalid.ReplaceAllString(name, "-")
	if len(v) > 63 {
		v = v[:63]
	}
	return strings.Trim(v, "-_.")
}

func validReservationName(name string) bool { return len(name) <= 63 }

// Reconcile implements reconcile.Reconciler.
func (r *GryviaReservationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	res := &gryviav1.GryviaReservation{}
	if err := r.Get(ctx, req.NamespacedName, res); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.sweepOrphans(ctx, "")
		}
		return ctrl.Result{}, err
	}
	if !res.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, res)
	}
	if !controllerutil.ContainsFinalizer(res, reservationFinalizer) {
		controllerutil.AddFinalizer(res, reservationFinalizer)
		if err := r.Update(ctx, res); err != nil {
			return ctrl.Result{}, err
		}
	}

	if res.Status.State == reservationStateExpired || res.Status.State == reservationStateCancelled {
		// Terminal: make sure nothing is still held (also repairs a crash between the state write and the release).
		return ctrl.Result{}, r.releaseAll(ctx, res.Name)
	}
	if !validReservationName(res.Name) {
		return ctrl.Result{}, r.setStatus(ctx, res, func(s *gryviav1.GryviaReservationStatus) {
			s.State = reservationStatePending
			s.Message = "reservation name longer than 63 characters cannot be used as a node label value; recreate it with a shorter name"
		})
	}

	result, err := r.reconcileReservation(ctx, res)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Reservations that vanished can leave labelled nodes behind (finalizer removed by hand).
	if err := r.sweepOrphans(ctx, res.Name); err != nil {
		log.FromContext(ctx).Error(err, "sweep orphaned reserved nodes")
	}
	return result, nil
}

type windowPhase int

const (
	windowPending windowPhase = iota
	windowActive
	windowEnded
)

// evalWindow decides whether the reservation's window is open at now and when to look again
// (zero: no scheduled change). An invalid schedule returns an error and stays pending.
func evalWindow(res *gryviav1.GryviaReservation, now time.Time) (windowPhase, time.Time, error) {
	sch := res.Spec.Schedule
	var end time.Time
	if sch.EndTime != nil {
		end = sch.EndTime.Time
	}
	ended := !end.IsZero() && !now.Before(end)

	switch sch.Type {
	case "immediate", "":
		if ended {
			return windowEnded, time.Time{}, nil
		}
		return windowActive, end, nil
	case "scheduled":
		if sch.StartTime == nil {
			return windowPending, time.Time{}, fmt.Errorf("schedule.startTime is required for a scheduled reservation")
		}
		if ended {
			return windowEnded, time.Time{}, nil
		}
		if now.Before(sch.StartTime.Time) {
			return windowPending, sch.StartTime.Time, nil
		}
		return windowActive, end, nil
	case "recurring":
		rc := sch.Recurrence
		if rc == nil || rc.Cron == "" || rc.Duration == "" {
			return windowPending, time.Time{}, fmt.Errorf("schedule.recurrence.cron and .duration are required for a recurring reservation")
		}
		cs, err := cron.Parse(rc.Cron)
		if err != nil {
			return windowPending, time.Time{}, err
		}
		dur, err := parseWindowDuration(rc.Duration)
		if err != nil {
			return windowPending, time.Time{}, err
		}
		if ended {
			return windowEnded, time.Time{}, nil
		}
		if sch.StartTime != nil && now.Before(sch.StartTime.Time) {
			return windowPending, sch.StartTime.Time, nil
		}
		bound := func(t time.Time) time.Time {
			if !end.IsZero() && (t.IsZero() || end.Before(t)) {
				return end
			}
			return t
		}
		if p, ok := cs.Prev(now, dur); ok && now.Before(p.Add(dur)) {
			return windowActive, bound(p.Add(dur)), nil
		}
		next, _ := cs.Next(now)
		return windowPending, bound(next), nil
	}
	return windowPending, time.Time{}, fmt.Errorf("unknown schedule type %q", sch.Type)
}

// parseWindowDuration accepts Go durations plus a leading day count: 8h, 90m, 2d, 1d12h.
func parseWindowDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var total time.Duration
	if i := strings.Index(s, "d"); i > 0 {
		n, err := strconv.Atoi(s[:i])
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		total = time.Duration(n) * 24 * time.Hour
		s = s[i+1:]
	}
	if s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration: %w", err)
		}
		total += d
	}
	if total <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return total, nil
}

func (r *GryviaReservationReconciler) reconcileReservation(ctx context.Context, res *gryviav1.GryviaReservation) (ctrl.Result, error) {
	now := r.now()
	phase, next, err := evalWindow(res, now)
	if err != nil {
		// Nothing is held for an invalid schedule.
		if rerr := r.releaseAll(ctx, res.Name); rerr != nil {
			return ctrl.Result{}, rerr
		}
		return ctrl.Result{RequeueAfter: reservationHeartbeat}, r.setStatus(ctx, res, func(s *gryviav1.GryviaReservationStatus) {
			s.State = reservationStatePending
			s.AllocatedNodes, s.AllocatedGPUs = nil, 0
			s.Message = "invalid schedule: " + err.Error()
		})
	}

	if phase == windowEnded && res.Spec.Schedule.AutoExtend {
		busy, err := r.hasRunningJobs(ctx, res)
		if err != nil {
			return ctrl.Result{}, err
		}
		if busy {
			phase = windowActive // keep the nodes while its jobs still run
		}
	}

	switch phase {
	case windowEnded:
		return ctrl.Result{}, r.finish(ctx, res, reservationStateExpired, "reservation ended")
	case windowPending:
		if err := r.releaseAll(ctx, res.Name); err != nil {
			return ctrl.Result{}, err
		}
		msg := "waiting for the reservation window"
		if !next.IsZero() {
			msg = "waiting for the reservation window (next start " + next.UTC().Format(time.RFC3339) + ")"
		}
		return r.requeueAt(now, next), r.setStatus(ctx, res, func(s *gryviav1.GryviaReservationStatus) {
			s.State = reservationStatePending
			s.AllocatedNodes, s.AllocatedGPUs = nil, 0
			s.Message = msg
		})
	}

	// Window active: hold the nodes.
	held, gpus, msg, err := r.allocate(ctx, res)
	if err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}
	jobs := r.countJobs(ctx, res)
	err = r.setStatus(ctx, res, func(s *gryviav1.GryviaReservationStatus) {
		if len(held) == 0 {
			s.State = reservationStatePending
		} else {
			if s.State != reservationStateActive || s.ActualStartTime == nil {
				t := metav1.NewTime(now)
				s.ActualStartTime = &t
			}
			s.State = reservationStateActive
		}
		s.AllocatedNodes, s.AllocatedGPUs, s.Message = held, gpus, msg
		if s.ActualStartTime != nil {
			if s.UtilizationMetrics == nil {
				s.UtilizationMetrics = &gryviav1.ReservationUtilization{}
			}
			s.UtilizationMetrics.TotalReservedTime = formatReservationDuration(now.Sub(s.ActualStartTime.Time))
			s.UtilizationMetrics.JobsRun = jobs
		}
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(held) == 0 {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return r.requeueAt(now, next), nil
}

// requeueAt asks for a reconcile at t (bounded by the heartbeat).
func (r *GryviaReservationReconciler) requeueAt(now, t time.Time) ctrl.Result {
	d := reservationHeartbeat
	if !t.IsZero() {
		if until := t.Sub(now) + time.Second; until > 0 && until < d {
			d = until
		}
	}
	return ctrl.Result{RequeueAfter: d}
}

func (r *GryviaReservationReconciler) setStatus(ctx context.Context, res *gryviav1.GryviaReservation, mutate func(*gryviav1.GryviaReservationStatus)) error {
	before := res.Status.DeepCopy()
	mutate(&res.Status)
	if equalStatus(before, &res.Status) {
		return nil
	}
	return r.Status().Update(ctx, res)
}

func equalStatus(a, b *gryviav1.GryviaReservationStatus) bool {
	if a.State != b.State || a.AllocatedGPUs != b.AllocatedGPUs || a.Message != b.Message ||
		(a.ActualStartTime == nil) != (b.ActualStartTime == nil) || (a.ActualEndTime == nil) != (b.ActualEndTime == nil) ||
		strings.Join(a.AllocatedNodes, ",") != strings.Join(b.AllocatedNodes, ",") {
		return false
	}
	au, bu := a.UtilizationMetrics, b.UtilizationMetrics
	if (au == nil) != (bu == nil) {
		return false
	}
	return au == nil || *au == *bu
}

// nodeGPUs is the GPU count of a node: nvidia.com/gpu allocatable, else the gryvia.io/gpu-count label.
func nodeGPUs(n *corev1.Node) int32 {
	if q, ok := n.Status.Allocatable[GPUResource]; ok && q.Value() > 0 {
		return int32(q.Value())
	}
	if v, err := strconv.ParseInt(n.Labels[LabelGPUCount], 10, 32); err == nil && v > 0 {
		return int32(v)
	}
	return 0
}

func nodeReady(n *corev1.Node) bool {
	if n.DeletionTimestamp != nil || n.Spec.Unschedulable {
		return false
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// selectNodes picks the nodes for a reservation from the cluster's nodes. It is pure and
// deterministic: only Ready, schedulable nodes not claimed by another reservation qualify;
// nodes this reservation already holds come first (so an allocation is stable), then nodes by
// name; it stops once the requested GPU count is covered. sufficient is false when the
// request cannot be met; the returned nodes are then the qualifying ones already held.
func selectNodes(nodes []corev1.Node, res *gryviav1.GryviaReservation) (chosen []string, gpus int32, sufficient bool, msg string) {
	rs := res.Spec.Resources
	usable := func(n *corev1.Node) bool {
		if !nodeReady(n) {
			return false
		}
		owner := n.Labels[LabelReservedFor]
		return owner == "" || owner == res.Name
	}
	byName := map[string]*corev1.Node{}
	for i := range nodes {
		byName[nodes[i].Name] = &nodes[i]
	}

	if len(rs.Nodes) > 0 { // explicit nodes: all of them or nothing new
		var bad []string
		for _, name := range rs.Nodes {
			n := byName[name]
			if n == nil || !usable(n) {
				bad = append(bad, name)
				continue
			}
			chosen = append(chosen, name)
			gpus += nodeGPUs(n)
		}
		sort.Strings(chosen)
		if len(bad) > 0 {
			held := heldOnly(nodes, res)
			return held, sumGPUs(held, byName), false, fmt.Sprintf("requested nodes not Ready, missing or reserved by another reservation: %s", strings.Join(bad, ", "))
		}
		return chosen, gpus, true, ""
	}
	if rs.GpuType == "" || rs.GpuCount <= 0 {
		return nil, 0, false, "resources need either nodes or gpuType and gpuCount"
	}

	var cands []*corev1.Node
	for i := range nodes {
		n := &nodes[i]
		if strings.EqualFold(n.Labels[LabelGPUType], rs.GpuType) && usable(n) && nodeGPUs(n) > 0 {
			cands = append(cands, n)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		hi, hj := cands[i].Labels[LabelReservedFor] == res.Name, cands[j].Labels[LabelReservedFor] == res.Name
		if hi != hj {
			return hi
		}
		return cands[i].Name < cands[j].Name
	})
	for _, n := range cands {
		if gpus >= rs.GpuCount {
			break
		}
		chosen = append(chosen, n.Name)
		gpus += nodeGPUs(n)
	}
	if gpus < rs.GpuCount {
		return heldOnlyFrom(cands, res), sumGPUs(heldOnlyFrom(cands, res), byName), false,
			fmt.Sprintf("only %d of %d requested %s GPUs are available on Ready, unreserved nodes", gpus, rs.GpuCount, rs.GpuType)
	}
	sort.Strings(chosen)
	return chosen, gpus, true, ""
}

func heldOnly(nodes []corev1.Node, res *gryviav1.GryviaReservation) []string {
	var out []string
	for i := range nodes {
		if nodes[i].Labels[LabelReservedFor] == res.Name && nodeReady(&nodes[i]) {
			out = append(out, nodes[i].Name)
		}
	}
	sort.Strings(out)
	return out
}

func heldOnlyFrom(cands []*corev1.Node, res *gryviav1.GryviaReservation) []string {
	var out []string
	for _, n := range cands {
		if n.Labels[LabelReservedFor] == res.Name {
			out = append(out, n.Name)
		}
	}
	sort.Strings(out)
	return out
}

func sumGPUs(names []string, byName map[string]*corev1.Node) int32 {
	var g int32
	for _, n := range names {
		g += nodeGPUs(byName[n])
	}
	return g
}

// allocate claims the selected nodes and releases nodes the reservation holds but no longer
// qualifies for (not Ready any more, or no longer selected). It returns what is held now.
func (r *GryviaReservationReconciler) allocate(ctx context.Context, res *gryviav1.GryviaReservation) (held []string, gpus int32, msg string, err error) {
	for attempt := 0; attempt < 3; attempt++ {
		nl := &corev1.NodeList{}
		if err = r.List(ctx, nl); err != nil {
			return nil, 0, "", fmt.Errorf("list nodes: %w", err)
		}
		chosen, g, sufficient, why := selectNodes(nl.Items, res)
		want := map[string]bool{}
		for _, n := range chosen {
			want[n] = true
		}
		// Release what we hold but no longer want.
		for i := range nl.Items {
			n := &nl.Items[i]
			if n.Labels[LabelReservedFor] == res.Name && !want[n.Name] {
				if err = r.release(ctx, n.Name, res.Name); err != nil {
					return nil, 0, "", err
				}
			}
		}
		if !sufficient && len(chosen) == 0 {
			return nil, 0, why, nil
		}
		// A partial allocation is only kept when nodes were already held; never hoard on the first try.
		lost := false
		for _, n := range chosen {
			if err = r.claim(ctx, n, res); err != nil {
				if errors.Is(err, errClaimedByOther) {
					lost = true // a concurrent reservation won this node: pick again
					break
				}
				return nil, 0, "", err
			}
		}
		if lost {
			continue
		}
		if !sufficient {
			return chosen, g, why, nil
		}
		return chosen, g, "", nil
	}
	return nil, 0, "nodes kept being claimed by other reservations; retrying", nil
}

// claim labels and taints a node for the reservation with an optimistic-locked patch, so two
// reservations racing for one node cannot both win: the loser gets a conflict, re-reads, and
// sees the other's label (errClaimedByOther).
func (r *GryviaReservationReconciler) claim(ctx context.Context, name string, res *gryviav1.GryviaReservation) error {
	owner := ReservationOwnerValue(res.Spec.Owner.Name)
	for i := 0; i < 5; i++ {
		n := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: name}, n); err != nil {
			return err
		}
		if cur := n.Labels[LabelReservedFor]; cur != "" && cur != res.Name {
			return errClaimedByOther
		}
		base := n.DeepCopy()
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		n.Labels[LabelReservedFor] = res.Name
		n.Labels[LabelReservedBy] = owner
		if res.Spec.Guarantees != nil && res.Spec.Guarantees.Exclusive {
			n.Labels[LabelExclusive] = "true"
		} else {
			delete(n.Labels, LabelExclusive)
		}
		n.Spec.Taints = setReservedTaint(n.Spec.Taints, owner)
		if equalNodeMeta(base, n) {
			return nil
		}
		err := r.Patch(ctx, n, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return fmt.Errorf("claim node %s: %w", name, err)
		}
	}
	return fmt.Errorf("claim node %s: too many conflicts", name)
}

func equalNodeMeta(a, b *corev1.Node) bool {
	if len(a.Labels) != len(b.Labels) || len(a.Spec.Taints) != len(b.Spec.Taints) {
		return false
	}
	for k, v := range a.Labels {
		if b.Labels[k] != v {
			return false
		}
	}
	for i := range a.Spec.Taints {
		if a.Spec.Taints[i] != b.Spec.Taints[i] {
			return false
		}
	}
	return true
}

// setReservedTaint sets the gryvia.io/reserved:NoSchedule taint to value, keeping other taints.
func setReservedTaint(taints []corev1.Taint, value string) []corev1.Taint {
	out := make([]corev1.Taint, 0, len(taints)+1)
	for _, t := range taints {
		if t.Key != ReservedTaintKey {
			out = append(out, t)
		}
	}
	return append(out, corev1.Taint{Key: ReservedTaintKey, Value: value, Effect: corev1.TaintEffectNoSchedule})
}

// release removes the taint and labels from a node claimed by the named reservation. A node
// claimed by anyone else (or by nobody) is left alone.
func (r *GryviaReservationReconciler) release(ctx context.Context, nodeName, reservation string) error {
	for i := 0; i < 5; i++ {
		n := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, n); err != nil {
			return client.IgnoreNotFound(err)
		}
		if n.Labels[LabelReservedFor] != reservation {
			return nil
		}
		base := n.DeepCopy()
		delete(n.Labels, LabelReservedFor)
		delete(n.Labels, LabelReservedBy)
		delete(n.Labels, LabelExclusive)
		kept := n.Spec.Taints[:0:0]
		for _, t := range n.Spec.Taints {
			if t.Key != ReservedTaintKey {
				kept = append(kept, t)
			}
		}
		n.Spec.Taints = kept
		err := r.Patch(ctx, n, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if err == nil || apierrors.IsNotFound(err) {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return fmt.Errorf("release node %s: %w", nodeName, err)
		}
	}
	return fmt.Errorf("release node %s: too many conflicts", nodeName)
}

// releaseAll releases every node labelled for the reservation (not only status.allocatedNodes,
// which can be stale after a crash).
func (r *GryviaReservationReconciler) releaseAll(ctx context.Context, reservation string) error {
	nl := &corev1.NodeList{}
	if err := r.List(ctx, nl, client.MatchingLabels{LabelReservedFor: reservation}); err != nil {
		return fmt.Errorf("list reserved nodes: %w", err)
	}
	for i := range nl.Items {
		if err := r.release(ctx, nl.Items[i].Name, reservation); err != nil {
			return err
		}
	}
	return nil
}

// sweepOrphans releases nodes whose reservation no longer exists or is terminal. skip names
// a reservation the caller is handling.
func (r *GryviaReservationReconciler) sweepOrphans(ctx context.Context, skip string) error {
	nl := &corev1.NodeList{}
	if err := r.List(ctx, nl, client.HasLabels{LabelReservedFor}); err != nil {
		return err
	}
	for i := range nl.Items {
		owner := nl.Items[i].Labels[LabelReservedFor]
		if owner == "" || owner == skip {
			continue
		}
		res := &gryviav1.GryviaReservation{}
		err := r.Get(ctx, types.NamespacedName{Name: owner}, res)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if apierrors.IsNotFound(err) || res.Status.State == reservationStateExpired ||
			res.Status.State == reservationStateCancelled || !res.DeletionTimestamp.IsZero() {
			if err := r.release(ctx, nl.Items[i].Name, owner); err != nil {
				return err
			}
		}
	}
	return nil
}

func jobUsesReservation(job *gryviav1.GryviaAIJob, name string) bool {
	return job.Annotations[annotationReservation] == name || job.Labels[annotationReservation] == name
}

func (r *GryviaReservationReconciler) reservationJobs(ctx context.Context, res *gryviav1.GryviaReservation) ([]gryviav1.GryviaAIJob, error) {
	list := &gryviav1.GryviaAIJobList{}
	if err := r.List(ctx, list); err != nil {
		return nil, err
	}
	var out []gryviav1.GryviaAIJob
	for _, j := range list.Items {
		if jobUsesReservation(&j, res.Name) {
			out = append(out, j)
		}
	}
	return out, nil
}

func (r *GryviaReservationReconciler) countJobs(ctx context.Context, res *gryviav1.GryviaReservation) int32 {
	jobs, err := r.reservationJobs(ctx, res)
	if err != nil {
		return 0
	}
	return int32(len(jobs))
}

func (r *GryviaReservationReconciler) hasRunningJobs(ctx context.Context, res *gryviav1.GryviaReservation) (bool, error) {
	jobs, err := r.reservationJobs(ctx, res)
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		if j.Status.Phase == "Running" || j.Status.Phase == "Scheduling" {
			return true, nil
		}
	}
	return false, nil
}

// finish releases the nodes, then records the terminal state (release first: a crash in
// between is repaired by the terminal-state branch of Reconcile).
func (r *GryviaReservationReconciler) finish(ctx context.Context, res *gryviav1.GryviaReservation, state, msg string) error {
	if err := r.releaseAll(ctx, res.Name); err != nil {
		return err
	}
	now := metav1.NewTime(r.now())
	return r.setStatus(ctx, res, func(s *gryviav1.GryviaReservationStatus) {
		s.State = state
		s.ActualEndTime = &now
		s.AllocatedNodes, s.AllocatedGPUs = nil, 0
		s.Message = msg
	})
}

func (r *GryviaReservationReconciler) handleDeletion(ctx context.Context, res *gryviav1.GryviaReservation) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(res, reservationFinalizer) {
		return ctrl.Result{}, nil
	}
	if err := r.releaseAll(ctx, res.Name); err != nil {
		return ctrl.Result{}, err // keep the finalizer until the nodes are clean
	}
	controllerutil.RemoveFinalizer(res, reservationFinalizer)
	return ctrl.Result{}, r.Update(ctx, res)
}

func formatReservationDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	hours := int(d.Hours())
	if hours < 24 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dd%dh", hours/24, hours%24)
}

// SetupWithManager sets up the controller with the Manager. Node changes (Ready flips,
// deletions) re-queue every live reservation.
func (r *GryviaReservationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("gryviareservation").
		For(&gryviav1.GryviaReservation{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
			list := &gryviav1.GryviaReservationList{}
			if err := mgr.GetClient().List(ctx, list); err != nil {
				return nil
			}
			var reqs []reconcile.Request
			for _, x := range list.Items {
				if x.Status.State != reservationStateExpired && x.Status.State != reservationStateCancelled {
					reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: x.Name}})
				}
			}
			return reqs
		})).
		Complete(r)
}
