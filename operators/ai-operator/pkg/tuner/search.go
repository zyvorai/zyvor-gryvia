// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package tuner

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// SearchStrategy generates hyperparameter combinations for trials.
type SearchStrategy interface {
	// GenerateCandidates returns the next batch of parameter sets to evaluate.
	// completedTrials contains results of previously completed trials.
	// count is the number of candidates requested.
	GenerateCandidates(space []gryviav1.ParameterSpec, completedTrials []gryviav1.TrialResult, count int) []map[string]string
}

// --- Grid Search ---------------------------------------------------------

// GridSearch exhaustively enumerates all parameter combinations.
type GridSearch struct{}

// GenerateCandidates returns up to count un-evaluated grid points.
func (g *GridSearch) GenerateCandidates(space []gryviav1.ParameterSpec, completedTrials []gryviav1.TrialResult, count int) []map[string]string {
	allCombinations := enumerateGrid(space)

	// Build a set of already-tried parameter combos for deduplication.
	tried := make(map[string]bool, len(completedTrials))
	for _, t := range completedTrials {
		tried[canonicalKey(t.Parameters)] = true
	}

	var candidates []map[string]string
	for _, combo := range allCombinations {
		if tried[canonicalKey(combo)] {
			continue
		}
		candidates = append(candidates, combo)
		if len(candidates) >= count {
			break
		}
	}
	return candidates
}

// enumerateGrid produces the Cartesian product of all parameter values.
func enumerateGrid(space []gryviav1.ParameterSpec) []map[string]string {
	paramValues := make([][]string, len(space))
	for i, p := range space {
		paramValues[i] = expandParam(p)
	}

	// Cartesian product via iterative expansion.
	var results []map[string]string
	results = append(results, map[string]string{})

	for i, p := range space {
		var next []map[string]string
		for _, prev := range results {
			for _, val := range paramValues[i] {
				combo := copyMap(prev)
				combo[p.Name] = val
				next = append(next, combo)
			}
		}
		results = next
	}
	return results
}

// --- Random Search -------------------------------------------------------

// RandomSearch samples parameters uniformly at random from the space.
type RandomSearch struct{}

// GenerateCandidates returns count random parameter sets.
func (rs *RandomSearch) GenerateCandidates(space []gryviav1.ParameterSpec, _ []gryviav1.TrialResult, count int) []map[string]string {
	candidates := make([]map[string]string, count)
	for i := 0; i < count; i++ {
		combo := make(map[string]string)
		for _, p := range space {
			combo[p.Name] = sampleParam(p)
		}
		candidates[i] = combo
	}
	return candidates
}

// --- Bayesian Search (TPE-inspired) --------------------------------------

// BayesianSearch uses a simplified Tree-structured Parzen Estimator approach.
// It partitions completed trials into "good" and "bad" sets based on the
// objective and samples new candidates that look more like the good set.
type BayesianSearch struct {
	// Direction is the optimization direction (minimize or maximize).
	Direction gryviav1.ObjectiveDirection
	// GammaQuantile is the fraction of trials considered "good" (default 0.25).
	GammaQuantile float64
}

// GenerateCandidates generates candidates biased toward high-performing regions.
func (b *BayesianSearch) GenerateCandidates(space []gryviav1.ParameterSpec, completedTrials []gryviav1.TrialResult, count int) []map[string]string {
	// Fall back to random search until we have enough data points.
	if len(completedTrials) < 4 {
		rs := &RandomSearch{}
		return rs.GenerateCandidates(space, completedTrials, count)
	}

	gamma := b.GammaQuantile
	if gamma <= 0 || gamma >= 1 {
		gamma = 0.25
	}

	// Partition trials into good and bad based on metric value.
	good, _ := partitionTrials(completedTrials, gamma, b.Direction)

	candidates := make([]map[string]string, count)
	for i := 0; i < count; i++ {
		combo := make(map[string]string)
		for _, p := range space {
			// 80% of the time sample from the good distribution; 20% explore.
			if rand.Float64() < 0.8 && len(good) > 0 {
				// Pick a random trial from the good set and perturb its value.
				donor := good[rand.Intn(len(good))]
				combo[p.Name] = perturbParam(p, donor.Parameters[p.Name])
			} else {
				combo[p.Name] = sampleParam(p)
			}
		}
		candidates[i] = combo
	}
	return candidates
}

// partitionTrials splits trials into good (top gamma fraction) and bad.
func partitionTrials(trials []gryviav1.TrialResult, gamma float64, direction gryviav1.ObjectiveDirection) (good, bad []gryviav1.TrialResult) {
	// Only consider trials with a metric value.
	var scored []gryviav1.TrialResult
	for _, t := range trials {
		if t.MetricValue != nil {
			scored = append(scored, t)
		}
	}
	if len(scored) == 0 {
		return nil, nil
	}

	sort.Slice(scored, func(i, j int) bool {
		if direction == gryviav1.ObjectiveMinimize {
			return *scored[i].MetricValue < *scored[j].MetricValue
		}
		return *scored[i].MetricValue > *scored[j].MetricValue
	})

	cutoff := int(math.Ceil(float64(len(scored)) * gamma))
	if cutoff < 1 {
		cutoff = 1
	}
	if cutoff > len(scored) {
		cutoff = len(scored)
	}
	return scored[:cutoff], scored[cutoff:]
}

// --- ASHA Scheduler (Asynchronous Successive Halving) --------------------

// ASHAScheduler implements aggressive early stopping by promoting only the
// top 1/reductionFactor trials at each rung.
type ASHAScheduler struct {
	MaxEpochs       int32
	ReductionFactor int32
	MinResource     int32
}

// ShouldStop returns true if the trial should be stopped at the given step
// based on its intermediate metrics compared to other trials at the same rung.
func (a *ASHAScheduler) ShouldStop(trial gryviav1.TrialResult, allTrials []gryviav1.TrialResult, direction gryviav1.ObjectiveDirection) bool {
	rf := a.ReductionFactor
	if rf <= 1 {
		rf = 3
	}
	minRes := a.MinResource
	if minRes <= 0 {
		minRes = 1
	}

	// Determine the current rung for this trial.
	currentStep := int32(0)
	var currentValue float64
	if len(trial.IntermediateMetrics) > 0 {
		last := trial.IntermediateMetrics[len(trial.IntermediateMetrics)-1]
		currentStep = last.Step
		currentValue = last.Value
	} else {
		return false
	}

	// Find the rung this step belongs to. Rungs are at minRes, minRes*rf, minRes*rf^2, ...
	rung := int32(0)
	for r := minRes; r <= a.MaxEpochs; r *= rf {
		if currentStep >= r {
			rung = r
		}
	}
	if rung == 0 {
		return false // haven't reached the first rung yet
	}

	// Collect all trial metrics at this rung.
	var metricsAtRung []float64
	for _, t := range allTrials {
		for _, m := range t.IntermediateMetrics {
			if m.Step >= rung {
				metricsAtRung = append(metricsAtRung, m.Value)
				break
			}
		}
	}

	if len(metricsAtRung) < 2 {
		return false // not enough data to make a decision
	}

	// Sort and determine the promotion cutoff.
	sort.Float64s(metricsAtRung)
	promotionCount := int(math.Ceil(float64(len(metricsAtRung)) / float64(rf)))
	if promotionCount < 1 {
		promotionCount = 1
	}

	// Check if this trial's value falls within the promoted set.
	if direction == gryviav1.ObjectiveMinimize {
		// Lower is better: promoted trials are the smallest values.
		threshold := metricsAtRung[promotionCount-1]
		return currentValue > threshold
	}
	// Higher is better: promoted trials are the largest values.
	threshold := metricsAtRung[len(metricsAtRung)-promotionCount]
	return currentValue < threshold
}

// --- Helpers -------------------------------------------------------------

// expandParam returns all discrete values for a parameter (used by grid search).
func expandParam(p gryviav1.ParameterSpec) []string {
	switch p.Type {
	case gryviav1.ParameterTypeCategorical:
		return p.Values

	case gryviav1.ParameterTypeInt:
		if len(p.Values) > 0 {
			return p.Values
		}
		if p.Min == nil || p.Max == nil {
			return nil
		}
		step := int32(1)
		if p.Step != nil && *p.Step > 0 {
			step = *p.Step
		}
		var vals []string
		for v := int32(*p.Min); v <= int32(*p.Max); v += step {
			vals = append(vals, strconv.FormatInt(int64(v), 10))
		}
		return vals

	case gryviav1.ParameterTypeFloat:
		if len(p.Values) > 0 {
			return p.Values
		}
		// For continuous parameters in grid search, discretize into 10 steps.
		if p.Min == nil || p.Max == nil {
			return nil
		}
		steps := 10
		vals := make([]string, 0, steps+1)
		for i := 0; i <= steps; i++ {
			var v float64
			frac := float64(i) / float64(steps)
			if p.Scale == "log" {
				v = math.Exp(math.Log(*p.Min) + frac*(math.Log(*p.Max)-math.Log(*p.Min)))
			} else {
				v = *p.Min + frac*(*p.Max-*p.Min)
			}
			vals = append(vals, strconv.FormatFloat(v, 'g', 6, 64))
		}
		return vals
	}
	return nil
}

// sampleParam returns a random value for a parameter.
func sampleParam(p gryviav1.ParameterSpec) string {
	switch p.Type {
	case gryviav1.ParameterTypeCategorical:
		if len(p.Values) == 0 {
			return ""
		}
		return p.Values[rand.Intn(len(p.Values))]

	case gryviav1.ParameterTypeInt:
		if len(p.Values) > 0 {
			return p.Values[rand.Intn(len(p.Values))]
		}
		if p.Min == nil || p.Max == nil {
			return "0"
		}
		step := int32(1)
		if p.Step != nil && *p.Step > 0 {
			step = *p.Step
		}
		lo := int32(*p.Min)
		hi := int32(*p.Max)
		rangeSteps := (hi - lo) / step
		if rangeSteps <= 0 {
			return strconv.FormatInt(int64(lo), 10)
		}
		v := lo + rand.Int31n(rangeSteps+1)*step
		return strconv.FormatInt(int64(v), 10)

	case gryviav1.ParameterTypeFloat:
		if len(p.Values) > 0 {
			return p.Values[rand.Intn(len(p.Values))]
		}
		if p.Min == nil || p.Max == nil {
			return "0"
		}
		var v float64
		if p.Scale == "log" {
			logMin := math.Log(*p.Min)
			logMax := math.Log(*p.Max)
			v = math.Exp(logMin + rand.Float64()*(logMax-logMin))
		} else {
			v = *p.Min + rand.Float64()*(*p.Max-*p.Min)
		}
		return strconv.FormatFloat(v, 'g', 6, 64)
	}
	return ""
}

// perturbParam generates a value near the given value for exploration.
func perturbParam(p gryviav1.ParameterSpec, val string) string {
	switch p.Type {
	case gryviav1.ParameterTypeCategorical:
		// For categorical, occasionally return the same value or a random one.
		if rand.Float64() < 0.5 {
			return val
		}
		return sampleParam(p)

	case gryviav1.ParameterTypeFloat:
		parsed, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return sampleParam(p)
		}
		if p.Min == nil || p.Max == nil {
			return val
		}
		// Perturb by up to 20% of the range.
		rangeSize := *p.Max - *p.Min
		perturbation := (rand.Float64() - 0.5) * 0.4 * rangeSize
		v := parsed + perturbation
		if v < *p.Min {
			v = *p.Min
		}
		if v > *p.Max {
			v = *p.Max
		}
		return strconv.FormatFloat(v, 'g', 6, 64)

	case gryviav1.ParameterTypeInt:
		parsed, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return sampleParam(p)
		}
		if p.Min == nil || p.Max == nil {
			return val
		}
		step := int64(1)
		if p.Step != nil && *p.Step > 0 {
			step = int64(*p.Step)
		}
		// Perturb by up to 2 steps in each direction.
		perturbSteps := rand.Int63n(5) - 2
		v := parsed + perturbSteps*step
		if v < int64(*p.Min) {
			v = int64(*p.Min)
		}
		if v > int64(*p.Max) {
			v = int64(*p.Max)
		}
		return strconv.FormatInt(v, 10)
	}
	return val
}

// canonicalKey produces a deterministic string key for a parameter map.
func canonicalKey(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, params[k]))
	}
	return strings.Join(parts, ",")
}

// copyMap returns a shallow copy of a string map.
func copyMap(m map[string]string) map[string]string {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// NewSearchStrategy returns the appropriate search strategy for the given algorithm.
func NewSearchStrategy(algo gryviav1.SearchAlgorithm, direction gryviav1.ObjectiveDirection) SearchStrategy {
	switch algo {
	case gryviav1.SearchAlgorithmGrid:
		return &GridSearch{}
	case gryviav1.SearchAlgorithmBayesian:
		return &BayesianSearch{Direction: direction, GammaQuantile: 0.25}
	case gryviav1.SearchAlgorithmASHA:
		// ASHA uses random search for candidate generation; early stopping is handled separately.
		return &RandomSearch{}
	default:
		return &RandomSearch{}
	}
}

// GridSize returns the number of combinations a grid search over space enumerates, saturating at limit+1 so a
// huge space is detected without computing it.
func GridSize(space []gryviav1.ParameterSpec, limit int) int {
	size := 1
	for _, p := range space {
		n := paramCount(p)
		if n == 0 {
			return 0
		}
		if size > limit/n {
			return limit + 1
		}
		size *= n
	}
	return size
}

// paramCount is len(expandParam(p)) without materialising a huge integer range.
func paramCount(p gryviav1.ParameterSpec) int {
	if p.Type == gryviav1.ParameterTypeInt && len(p.Values) == 0 {
		if p.Min == nil || p.Max == nil || *p.Max < *p.Min {
			return 0
		}
		step := 1.0
		if p.Step != nil && *p.Step > 0 {
			step = float64(*p.Step)
		}
		n := math.Floor((*p.Max-*p.Min)/step) + 1
		if n > 1<<30 {
			return 1 << 30
		}
		return int(n)
	}
	return len(expandParam(p))
}
