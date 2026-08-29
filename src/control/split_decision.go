package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// A split is only worth it when it is worth it.
//
// Merc's thesis is that unequal machines contribute to the same project by
// comparative advantage. The discipline that makes that honest is the refusal:
// distribution is valid ONLY when useful compute gained exceeds startup +
// movement + assembly + verification cost. Splitting work that should not be
// split is a loss dressed up as parallelism.
//
// This file is the auditable arithmetic for that rule. It does not schedule,
// price, or dispatch. It answers one question about one divisible workload and
// one candidate set: should we split, and if we should, how should the shards
// fall.
//
// Every input must be a measurement (or a named observation such as "already
// resident") with a citation. planner.go still fills missing cold-load with a
// 120s default and a 2s chunk overhead; those are estimates, and an estimate
// is not an input this decision will use. Missing evidence is a REFUSED
// result, not a guess.

const splitDecisionVersion = 1

const (
	splitDecisionAccepted = "ACCEPTED"
	splitDecisionRefused  = "REFUSED"
)

const (
	splitPlanMatched = "MATCHED_TO_STRENGTH"
	splitPlanEqual   = "EQUAL_SHARDS"
)

// Named refusal terms. Overhead refusals name the cost that dominated the
// inequality. lack_of_evidence means a required measurement was absent, not
// that the arithmetic ran and lost. no_useful_compute means the shard plan
// did not beat a single strongest worker even before overhead.
const (
	splitKilledStartup      = "startup"
	splitKilledMovement     = "movement"
	splitKilledAssembly     = "assembly"
	splitKilledVerification = "verification"
	splitKilledNoGain       = "no_useful_compute"
	splitKilledEvidence     = "lack_of_evidence"
)

// SplitMeasuredSecs is one duration the arithmetic is allowed to use.
//
// Evidence must name the receipt, snapshot, or observation the number came
// from. Zero is a legal measurement (already-resident, no extra assembly). A
// missing Evidence is not a zero: it is a refusal for lack of evidence.
type SplitMeasuredSecs struct {
	Secs     float64
	Evidence string
}

func (m SplitMeasuredSecs) present() bool {
	return strings.TrimSpace(m.Evidence) != "" &&
		!math.IsNaN(m.Secs) && !math.IsInf(m.Secs, 0) && m.Secs >= 0
}

// SplitWorkload is a divisible job. Units and payload bytes are counted, not
// estimated: they are the same class of fact ComputePlan.InputRecords /
// InputBytes already freeze.
type SplitWorkload struct {
	Units           int64
	PayloadBytes    int64
	PayloadEvidence string
}

// SplitWorker is one candidate. UnitsPerSec is measured throughput on this
// cell/profile; Startup is measured model-load / dispatch for this worker.
// Empty evidence, a non-positive rate, or an unmeasured startup refuses the
// plan rather than substituting plannerDefaultColdLoadSecs.
type SplitWorker struct {
	ID                 string
	UnitsPerSec        float64
	ThroughputEvidence string
	Startup            SplitMeasuredSecs
}

// SplitCostEvidence is the overhead side of the inequality. Assembly and
// verification are extra-versus-single-worker durations for this candidate
// set (same shard count for matched and equal plans).
//
// DuplicateBytes is the measured artifact each additional worker must hold
// that the single-worker plan would not recopy (model weights, shared
// assets). Zero with evidence means already resident. A positive copy with
// no measured transfer rate is lack of evidence, not an assumed Gbps.
type SplitCostEvidence struct {
	DuplicateBytes      int64
	DuplicateEvidence   string
	TransferBytesPerSec float64
	TransferEvidence    string
	Assembly            SplitMeasuredSecs
	Verification        SplitMeasuredSecs
}

// SplitShard is one worker's piece of a proposed plan.
type SplitShard struct {
	WorkerID    string  `json:"worker_id"`
	Units       int64   `json:"units"`
	Bytes       int64   `json:"bytes"`
	ComputeSecs float64 `json:"compute_secs"`
}

// SplitArithmetic is the inequality, term by term, so a reader can see why
// a plan was accepted or which cost killed it.
type SplitArithmetic struct {
	SerialComputeSecs   float64 `json:"serial_compute_secs"`
	ParallelComputeSecs float64 `json:"parallel_compute_secs"`
	UsefulComputeSecs   float64 `json:"useful_compute_secs"`
	StartupSecs         float64 `json:"startup_secs"`
	MovementSecs        float64 `json:"movement_secs"`
	AssemblySecs        float64 `json:"assembly_secs"`
	VerificationSecs    float64 `json:"verification_secs"`
	OverheadSecs        float64 `json:"overhead_secs"`
	NetSecs             float64 `json:"net_secs"`
	StrongestWorkerID   string  `json:"strongest_worker_id"`
	StrongestRate       float64 `json:"strongest_rate"`
}

// WorthIt is the governing inequality: useful compute gained strictly
// exceeds the sum of the four overhead terms.
func (a SplitArithmetic) WorthIt() bool {
	return a.UsefulComputeSecs > a.OverheadSecs
}

// SplitPlan is one shard assignment plus the arithmetic that scored it.
type SplitPlan struct {
	Kind       string          `json:"kind"`
	Shards     []SplitShard    `json:"shards"`
	Arithmetic SplitArithmetic `json:"arithmetic"`
}

// SplitDecision is the first-class result. REFUSED is a result, not an error.
type SplitDecision struct {
	Version  int       `json:"version"`
	Status   string    `json:"status"`
	Reason   string    `json:"reason"`
	KilledBy string    `json:"killed_by,omitempty"`
	Missing  string    `json:"missing,omitempty"`
	Chosen   SplitPlan `json:"chosen,omitempty"`
	Rival    SplitPlan `json:"rival,omitempty"`
	Evidence []string  `json:"evidence,omitempty"`
}

// DecideSplit evaluates whether distributing workload across workers is
// worth the overhead. The proposed plan always matches shard size to
// measured strength. Equal shards are scored on the same inputs as the
// naive rival: when workers are unequal they lose, which is the point.
func DecideSplit(workload SplitWorkload, workers []SplitWorker, cost SplitCostEvidence) SplitDecision {
	out := SplitDecision{Version: splitDecisionVersion, Status: splitDecisionRefused}
	if missing := splitEvidenceGap(workload, workers, cost); missing != "" {
		out.KilledBy = splitKilledEvidence
		out.Missing = missing
		out.Reason = fmt.Sprintf(
			"refused for lack of evidence: %s is not measured at this commit; the plan will not estimate it",
			missing)
		return out
	}

	ordered := append([]SplitWorker(nil), workers...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	matched := buildSplitPlan(splitPlanMatched, workload, ordered, strengthWeights(ordered))
	equal := buildSplitPlan(splitPlanEqual, workload, ordered, equalWeights(len(ordered)))
	matched.Arithmetic = scoreSplitPlan(workload, ordered, matched.Shards, cost)
	equal.Arithmetic = scoreSplitPlan(workload, ordered, equal.Shards, cost)
	out.Chosen = matched
	out.Rival = equal
	out.Evidence = splitEvidenceCitations(workload, ordered, cost)

	if !matched.Arithmetic.WorthIt() {
		killed, secs := splitDominatingTerm(matched.Arithmetic)
		out.KilledBy = killed
		out.Reason = fmt.Sprintf(
			"refused: useful compute gained %.6fs does not exceed overhead %.6fs "+
				"(startup=%.6f movement=%.6f assembly=%.6f verification=%.6f); dominating term is %s (%.6fs)",
			matched.Arithmetic.UsefulComputeSecs, matched.Arithmetic.OverheadSecs,
			matched.Arithmetic.StartupSecs, matched.Arithmetic.MovementSecs,
			matched.Arithmetic.AssemblySecs, matched.Arithmetic.VerificationSecs,
			killed, secs)
		return out
	}

	out.Status = splitDecisionAccepted
	out.Reason = fmt.Sprintf(
		"accepted matched split: useful compute gained %.6fs exceeds overhead %.6fs "+
			"(startup=%.6f movement=%.6f assembly=%.6f verification=%.6f); net %.6fs",
		matched.Arithmetic.UsefulComputeSecs, matched.Arithmetic.OverheadSecs,
		matched.Arithmetic.StartupSecs, matched.Arithmetic.MovementSecs,
		matched.Arithmetic.AssemblySecs, matched.Arithmetic.VerificationSecs,
		matched.Arithmetic.NetSecs)
	return out
}

func splitEvidenceGap(workload SplitWorkload, workers []SplitWorker, cost SplitCostEvidence) string {
	if workload.Units <= 0 {
		return "workload.units"
	}
	if strings.TrimSpace(workload.PayloadEvidence) == "" || workload.PayloadBytes < 0 {
		return "workload.payload_bytes"
	}
	if len(workers) < 2 {
		return "workers (need at least two candidates to split)"
	}
	seen := make(map[string]struct{}, len(workers))
	for i, w := range workers {
		if strings.TrimSpace(w.ID) == "" {
			return fmt.Sprintf("workers[%d].id", i)
		}
		if _, dup := seen[w.ID]; dup {
			return fmt.Sprintf("workers[%d].id (duplicate %q)", i, w.ID)
		}
		seen[w.ID] = struct{}{}
		if strings.TrimSpace(w.ThroughputEvidence) == "" ||
			w.UnitsPerSec <= 0 || math.IsNaN(w.UnitsPerSec) || math.IsInf(w.UnitsPerSec, 0) {
			return fmt.Sprintf("workers[%d].throughput (%s)", i, w.ID)
		}
		if !w.Startup.present() {
			return fmt.Sprintf("workers[%d].startup (%s)", i, w.ID)
		}
	}
	if strings.TrimSpace(cost.DuplicateEvidence) == "" || cost.DuplicateBytes < 0 {
		return "cost.duplicate_bytes"
	}
	if cost.DuplicateBytes > 0 {
		if strings.TrimSpace(cost.TransferEvidence) == "" ||
			cost.TransferBytesPerSec <= 0 || math.IsNaN(cost.TransferBytesPerSec) ||
			math.IsInf(cost.TransferBytesPerSec, 0) {
			return "cost.transfer_bytes_per_sec"
		}
	}
	if !cost.Assembly.present() {
		return "cost.assembly"
	}
	if !cost.Verification.present() {
		return "cost.verification"
	}
	return ""
}

func splitEvidenceCitations(workload SplitWorkload, workers []SplitWorker, cost SplitCostEvidence) []string {
	out := []string{workload.PayloadEvidence}
	for _, w := range workers {
		out = append(out, w.ThroughputEvidence, w.Startup.Evidence)
	}
	out = append(out, cost.DuplicateEvidence, cost.Assembly.Evidence, cost.Verification.Evidence)
	if cost.DuplicateBytes > 0 {
		out = append(out, cost.TransferEvidence)
	}
	return out
}

func strengthWeights(workers []SplitWorker) []float64 {
	out := make([]float64, len(workers))
	for i, w := range workers {
		out[i] = w.UnitsPerSec
	}
	return out
}

func equalWeights(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = 1
	}
	return out
}

func buildSplitPlan(kind string, workload SplitWorkload, workers []SplitWorker, weights []float64) SplitPlan {
	units := proportionInt(workload.Units, weights)
	bytes := make([]int64, len(workers))
	if workload.PayloadBytes > 0 && workload.Units > 0 {
		byteWeights := make([]float64, len(workers))
		for i := range workers {
			byteWeights[i] = float64(units[i])
		}
		bytes = proportionInt(workload.PayloadBytes, byteWeights)
	}
	shards := make([]SplitShard, 0, len(workers))
	for i, w := range workers {
		compute := 0.0
		if units[i] > 0 {
			compute = float64(units[i]) / w.UnitsPerSec
		}
		shards = append(shards, SplitShard{
			WorkerID:    w.ID,
			Units:       units[i],
			Bytes:       bytes[i],
			ComputeSecs: compute,
		})
	}
	return SplitPlan{Kind: kind, Shards: shards}
}

// proportionInt apportions total across weights by largest remainder so the
// parts sum exactly. Equal weights yield equal parts (the naive plan).
func proportionInt(total int64, weights []float64) []int64 {
	n := len(weights)
	out := make([]int64, n)
	if total <= 0 || n == 0 {
		return out
	}
	var sumW float64
	for _, w := range weights {
		if w > 0 && !math.IsNaN(w) && !math.IsInf(w, 0) {
			sumW += w
		}
	}
	if sumW <= 0 {
		return out
	}
	type remainder struct {
		i int
		r float64
	}
	rems := make([]remainder, 0, n)
	var given int64
	for i, w := range weights {
		if w <= 0 || math.IsNaN(w) || math.IsInf(w, 0) {
			continue
		}
		exact := float64(total) * w / sumW
		whole := int64(math.Floor(exact + 1e-12))
		out[i] = whole
		given += whole
		rems = append(rems, remainder{i: i, r: exact - float64(whole)})
	}
	leftover := total - given
	sort.SliceStable(rems, func(a, b int) bool {
		if rems[a].r != rems[b].r {
			return rems[a].r > rems[b].r
		}
		return rems[a].i < rems[b].i
	})
	for leftover > 0 && len(rems) > 0 {
		for i := 0; i < len(rems) && leftover > 0; i++ {
			out[rems[i].i]++
			leftover--
		}
	}
	for leftover < 0 {
		trimmed := false
		for i := len(out) - 1; i >= 0 && leftover < 0; i-- {
			if out[i] > 0 {
				out[i]--
				leftover++
				trimmed = true
			}
		}
		if !trimmed {
			break
		}
	}
	return out
}

func scoreSplitPlan(workload SplitWorkload, workers []SplitWorker, shards []SplitShard, cost SplitCostEvidence) SplitArithmetic {
	byID := make(map[string]SplitWorker, len(workers))
	strongest := workers[0]
	for _, w := range workers {
		byID[w.ID] = w
		if w.UnitsPerSec > strongest.UnitsPerSec {
			strongest = w
		}
	}
	serial := float64(workload.Units) / strongest.UnitsPerSec
	var parallel float64
	var startup float64
	active := 0
	strongestActive := false
	for _, sh := range shards {
		if sh.Units <= 0 {
			continue
		}
		active++
		if sh.ComputeSecs > parallel {
			parallel = sh.ComputeSecs
		}
		w := byID[sh.WorkerID]
		startup += w.Startup.Secs
		if w.ID == strongest.ID {
			strongestActive = true
		}
	}
	if strongestActive {
		startup -= strongest.Startup.Secs
		if startup < 0 {
			startup = 0
		}
	}
	var movement float64
	if cost.DuplicateBytes > 0 && active > 1 && cost.TransferBytesPerSec > 0 {
		movement = float64(active-1) * float64(cost.DuplicateBytes) / cost.TransferBytesPerSec
	}
	assembly := cost.Assembly.Secs
	verification := cost.Verification.Secs
	if active < 2 {
		// A plan that collapsed onto one worker is not a split: extra
		// assembly/verification of a fan-out that did not happen is zero, and
		// there is no useful compute to claim.
		assembly, verification, movement, startup = 0, 0, 0, 0
		parallel = serial
	}
	useful := serial - parallel
	overhead := startup + movement + assembly + verification
	return SplitArithmetic{
		SerialComputeSecs:   serial,
		ParallelComputeSecs: parallel,
		UsefulComputeSecs:   useful,
		StartupSecs:         startup,
		MovementSecs:        movement,
		AssemblySecs:        assembly,
		VerificationSecs:    verification,
		OverheadSecs:        overhead,
		NetSecs:             useful - overhead,
		StrongestWorkerID:   strongest.ID,
		StrongestRate:       strongest.UnitsPerSec,
	}
}

func splitDominatingTerm(a SplitArithmetic) (string, float64) {
	if a.UsefulComputeSecs <= 0 {
		return splitKilledNoGain, a.UsefulComputeSecs
	}
	type term struct {
		name string
		secs float64
	}
	terms := []term{
		{splitKilledStartup, a.StartupSecs},
		{splitKilledMovement, a.MovementSecs},
		{splitKilledAssembly, a.AssemblySecs},
		{splitKilledVerification, a.VerificationSecs},
	}
	best := terms[0]
	for _, t := range terms[1:] {
		if t.secs > best.secs {
			best = t
		}
	}
	return best.name, best.secs
}
