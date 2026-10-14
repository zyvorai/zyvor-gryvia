package scheduler

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

const MilliGPU int64 = 1000

var (
	ErrQueuePolicy      = errors.New("invalid queue policy")
	ErrQuotaExceeded    = errors.New("GPU quota exceeded")
	ErrGangUnsatisfied  = errors.New("gang cannot be admitted")
	ErrNoPreemptionPlan = errors.New("no safe preemption plan")
)

type NativeQueuePolicy struct {
	Name         string
	GuaranteeGPU int64 // milli-GPU
	MaxGPU       int64 // milli-GPU; 0 = no explicit ceiling
	Weight       float64
	Borrow       bool
	Preemptible  bool
}

type NativeWorkload struct {
	ID           string
	Queue        string
	Priority     int32
	CreatedAt    time.Time
	Replicas     int
	MinAvailable int
	GPUPerPod    int64 // milli-GPU
	Preemptible  bool
}

type NativeAllocation struct {
	WorkloadID  string
	Queue       string
	Priority    int32
	GPU         int64
	Preemptible bool
}

type QueueUsage struct {
	UsedGPU       int64
	GuaranteedGPU int64
	MaxGPU        int64
	Weight        float64
	DominantShare float64
}

type PreemptionVictim struct {
	WorkloadID string
	Queue      string
	Priority   int32
	GPU        int64
}

type AdmissionPlan struct {
	Allow       bool
	NeedGPU     int64
	BorrowedGPU int64
	Victims     []PreemptionVictim
	Reason      string
}

type NativeQueueEngine struct {
	Policies map[string]NativeQueuePolicy
}

func NewNativeQueueEngine(policies []NativeQueuePolicy) (*NativeQueueEngine, error) {
	e := &NativeQueueEngine{Policies: map[string]NativeQueuePolicy{}}
	for _, p := range policies {
		if p.Name == "" || p.GuaranteeGPU < 0 || p.MaxGPU < 0 {
			return nil, fmt.Errorf("%w: bad policy for %q", ErrQueuePolicy, p.Name)
		}
		if p.MaxGPU > 0 && p.GuaranteeGPU > p.MaxGPU {
			return nil, fmt.Errorf("%w: guarantee exceeds max for %s", ErrQueuePolicy, p.Name)
		}
		if p.Weight <= 0 {
			p.Weight = 1
		}
		e.Policies[p.Name] = p
	}
	return e, nil
}

func normalizeNativeWorkload(w NativeWorkload) (NativeWorkload, error) {
	if w.ID == "" || w.Queue == "" || w.Replicas <= 0 || w.GPUPerPod <= 0 {
		return w, fmt.Errorf("%w: workload fields", ErrQueuePolicy)
	}
	if w.MinAvailable == 0 {
		w.MinAvailable = w.Replicas
	}
	if w.MinAvailable < 1 || w.MinAvailable > w.Replicas {
		return w, fmt.Errorf("%w: minAvailable", ErrQueuePolicy)
	}
	if w.CreatedAt.IsZero() {
		w.CreatedAt = time.Now()
	}
	return w, nil
}

func usageByQueue(allocs []NativeAllocation) map[string]int64 {
	out := map[string]int64{}
	for _, a := range allocs {
		if a.GPU > 0 {
			out[a.Queue] += a.GPU
		}
	}
	return out
}

func dominantShare(policy NativeQueuePolicy, used int64) float64 {
	den := policy.GuaranteeGPU
	if den <= 0 {
		den = policy.MaxGPU
	}
	if den <= 0 {
		den = MilliGPU
	}
	weight := policy.Weight
	if weight <= 0 {
		weight = 1
	}
	share := float64(used) / float64(den)
	// Queues still below guarantee get an explicit boost.
	if policy.GuaranteeGPU > 0 && used < policy.GuaranteeGPU {
		share *= 0.5
	}
	return share / weight
}

func (e *NativeQueueEngine) Snapshot(allocs []NativeAllocation) map[string]QueueUsage {
	used := usageByQueue(allocs)
	out := make(map[string]QueueUsage, len(e.Policies))
	for name, p := range e.Policies {
		out[name] = QueueUsage{
			UsedGPU: used[name], GuaranteedGPU: p.GuaranteeGPU, MaxGPU: p.MaxGPU,
			Weight: p.Weight, DominantShare: dominantShare(p, used[name]),
		}
	}
	return out
}

// Order implements priority first, weighted dominant-resource fairness second, FIFO third.
// This is deliberately deterministic so the controller can recompute after restarts.
func (e *NativeQueueEngine) Order(pending []NativeWorkload, allocs []NativeAllocation) ([]NativeWorkload, error) {
	used := usageByQueue(allocs)
	out := append([]NativeWorkload(nil), pending...)
	for i := range out {
		w, err := normalizeNativeWorkload(out[i])
		if err != nil {
			return nil, err
		}
		if _, ok := e.Policies[w.Queue]; !ok {
			return nil, fmt.Errorf("%w: queue %q", ErrQueuePolicy, w.Queue)
		}
		out[i] = w
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		pi, pj := e.Policies[out[i].Queue], e.Policies[out[j].Queue]
		si, sj := dominantShare(pi, used[out[i].Queue]), dominantShare(pj, used[out[j].Queue])
		if math.Abs(si-sj) > 1e-12 {
			return si < sj
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// PlanAdmission enforces minAvailable atomically. freeGPU is cluster-wide schedulable capacity
// after node filtering/holds. Existing placement still decides WHICH nodes.
func (e *NativeQueueEngine) PlanAdmission(w NativeWorkload, allocs []NativeAllocation, freeGPU int64) (AdmissionPlan, error) {
	var err error
	w, err = normalizeNativeWorkload(w)
	if err != nil {
		return AdmissionPlan{}, err
	}
	p, ok := e.Policies[w.Queue]
	if !ok {
		return AdmissionPlan{}, fmt.Errorf("%w: queue %q", ErrQueuePolicy, w.Queue)
	}

	used := usageByQueue(allocs)
	minNeed := int64(w.MinAvailable) * w.GPUPerPod
	fullNeed := int64(w.Replicas) * w.GPUPerPod
	if p.MaxGPU > 0 && used[w.Queue]+minNeed > p.MaxGPU {
		return AdmissionPlan{Reason: "queue max GPU exceeded"}, ErrQuotaExceeded
	}
	if freeGPU >= minNeed {
		admit := fullNeed
		if admit > freeGPU {
			admit = freeGPU
		}
		borrowed := int64(0)
		after := used[w.Queue] + admit
		if after > p.GuaranteeGPU && p.GuaranteeGPU > 0 {
			if !p.Borrow && after > p.GuaranteeGPU {
				admit = p.GuaranteeGPU - used[w.Queue]
				if admit < minNeed {
					return AdmissionPlan{Reason: "borrowing disabled and guarantee exhausted"}, ErrGangUnsatisfied
				}
			} else {
				borrowed = after - p.GuaranteeGPU
			}
		}
		return AdmissionPlan{Allow: true, NeedGPU: admit, BorrowedGPU: borrowed, Reason: "admitted"}, nil
	}

	// Native preemption is conservative: only lower-priority, explicitly preemptible
	// allocations are candidates. At equal priority we never evict.
	var candidates []PreemptionVictim
	for _, a := range allocs {
		if !a.Preemptible || a.Priority >= w.Priority || a.GPU <= 0 {
			continue
		}
		pp := e.Policies[a.Queue]
		// Preserve another queue's guarantee unless the incoming workload is itself
		// trying to reach its guarantee. This prevents aggressive cross-team eviction.
		if used[a.Queue]-a.GPU < pp.GuaranteeGPU && used[w.Queue] >= p.GuaranteeGPU {
			continue
		}
		candidates = append(candidates, PreemptionVictim{
			WorkloadID: a.WorkloadID, Queue: a.Queue, Priority: a.Priority, GPU: a.GPU,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority < candidates[j].Priority
		}
		pi, pj := e.Policies[candidates[i].Queue], e.Policies[candidates[j].Queue]
		return dominantShare(pi, used[candidates[i].Queue]) > dominantShare(pj, used[candidates[j].Queue])
	})

	reclaim := int64(0)
	var victims []PreemptionVictim
	for _, v := range candidates {
		victims = append(victims, v)
		reclaim += v.GPU
		if freeGPU+reclaim >= minNeed {
			break
		}
	}
	if freeGPU+reclaim < minNeed {
		return AdmissionPlan{Reason: "insufficient capacity after safe preemption"}, ErrNoPreemptionPlan
	}
	return AdmissionPlan{Allow: true, NeedGPU: minNeed, Victims: victims, Reason: "admitted with preemption"}, nil
}
