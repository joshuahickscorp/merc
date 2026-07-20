package main

import (
	"os"
	"sort"
	"sync/atomic"

	"github.com/google/uuid"
)

var fanoutPlannerEnabled atomic.Bool

func init() {
	fanoutPlannerEnabled.Store(os.Getenv("CX_DISABLE_FANOUT_PLANNER") == "")
}

const (
	plannerChunkOverheadSecs = 2.0

	plannerDefaultColdLoadSecs = 120.0

	plannerConservativeRateFactor = 0.75

	plannerMinFleetSamples = 3
)

type PlannerWorker struct {
	ID           uuid.UUID
	ItemsPerSec  float64
	Warm         bool
	ColdLoadSecs float64
	Throttled    bool
}

func (w PlannerWorker) startCostSecs() float64 {
	c := plannerChunkOverheadSecs
	if !w.Warm {
		c += w.ColdLoadSecs
	}
	return c
}

type PlannerJob struct {
	Items    int
	JobType  string
	ModelRef string
}

type PlannerAssignment struct {
	WorkerID     uuid.UUID
	Items        int
	ExpectedSecs float64 // startCost + Items/rate, modeled
}

type Plan struct {
	Width                     int                 // recommended fan-out width (workers with Items > 0)
	Assignments               []PlannerAssignment // per-worker shares, deterministic order
	WallClockP50Secs          float64
	WallClockConservativeSecs float64
	SingleNodeSecs            float64
	ModeledSpeedupVsSingle    float64
}

func planCapacity(ws []PlannerWorker, t float64) float64 {
	var c float64
	for _, w := range ws {
		if dt := t - w.startCostSecs(); dt > 0 {
			c += dt * w.ItemsPerSec
		}
	}
	return c
}

func PlanFanout(job PlannerJob, fleet []PlannerWorker) Plan {
	ws := make([]PlannerWorker, 0, len(fleet))
	for _, w := range fleet {
		if w.Throttled || w.ItemsPerSec <= 0 {
			continue
		}
		ws = append(ws, w)
	}
	if job.Items <= 0 || len(ws) == 0 {
		return Plan{}
	}
	sort.Slice(ws, func(i, j int) bool {
		si, sj := ws[i].startCostSecs(), ws[j].startCostSecs()
		if si != sj {
			return si < sj
		}
		if ws[i].ItemsPerSec != ws[j].ItemsPerSec {
			return ws[i].ItemsPerSec > ws[j].ItemsPerSec
		}
		return ws[i].ID.String() < ws[j].ID.String()
	})

	items := float64(job.Items)

	single := singleNodeSecs(ws, items)

	lo, hi := 0.0, single
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		if planCapacity(ws, mid) >= items {
			hi = mid
		} else {
			lo = mid
		}
	}
	tStar := hi

	type share struct {
		idx   int
		whole int
		frac  float64
	}
	shares := make([]share, len(ws))
	assignedWhole := 0
	for i, w := range ws {
		f := 0.0
		if dt := tStar - w.startCostSecs(); dt > 0 {
			f = dt * w.ItemsPerSec
		}
		wh := int(f)
		shares[i] = share{idx: i, whole: wh, frac: f - float64(wh)}
		assignedWhole += wh
	}
	remainder := job.Items - assignedWhole
	if remainder < 0 {
		remainder = 0
		total := 0
		for i := range shares {
			total += shares[i].whole
		}
		for i := len(shares) - 1; i >= 0 && total > job.Items; i-- {
			trim := total - job.Items
			if trim > shares[i].whole {
				trim = shares[i].whole
			}
			shares[i].whole -= trim
			total -= trim
		}
	}
	if remainder > 0 {
		var order []int
		for i := range shares {
			if shares[i].frac > 0 || shares[i].whole > 0 {
				order = append(order, i)
			}
		}
		if len(order) == 0 {
			order = []int{0} // degenerate: give everything to the cheapest-start worker
		}
		sort.SliceStable(order, func(a, b int) bool {
			return shares[order[a]].frac > shares[order[b]].frac
		})
		for r := 0; r < remainder; r++ {
			shares[order[r%len(order)]].whole++
		}
	}

	var plan Plan
	for _, sh := range shares {
		if sh.whole <= 0 {
			continue
		}
		w := ws[sh.idx]
		exp := w.startCostSecs() + float64(sh.whole)/w.ItemsPerSec
		plan.Assignments = append(plan.Assignments, PlannerAssignment{
			WorkerID: w.ID, Items: sh.whole, ExpectedSecs: exp,
		})
		if exp > plan.WallClockP50Secs {
			plan.WallClockP50Secs = exp
		}
		cons := w.startCostSecs() + float64(sh.whole)/(w.ItemsPerSec*plannerConservativeRateFactor)
		if cons > plan.WallClockConservativeSecs {
			plan.WallClockConservativeSecs = cons
		}
	}
	plan.Width = len(plan.Assignments)
	plan.SingleNodeSecs = single
	if plan.WallClockP50Secs > 0 {
		plan.ModeledSpeedupVsSingle = plan.SingleNodeSecs / plan.WallClockP50Secs
	}
	return plan
}

func singleNodeSecs(ws []PlannerWorker, items float64) float64 {
	best := 0.0
	for i, w := range ws {
		t := w.startCostSecs() + items/w.ItemsPerSec
		if i == 0 || t < best {
			best = t
		}
	}
	return best
}

func medianRate(rates []float64) float64 {
	if len(rates) == 0 {
		return 0
	}
	s := append([]float64(nil), rates...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func rankPeersBySpeed(ranked []MatchWorker, jobType string) []MatchWorker {
	out := append([]MatchWorker(nil), ranked...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Warm != out[j].Warm {
			return out[i].Warm
		}
		return out[i].TPS[jobType] > out[j].TPS[jobType]
	})
	return out
}

func tokensPerItemEstimate(maxTokens uint32, avgLineBytes float64) float64 {
	out := float64(maxTokens)
	if out <= 0 {
		out = defaultQuoteMaxTokens
	}
	prompt := 0.0
	if avgLineBytes > 0 {
		prompt = avgLineBytes / 4.0
	}
	t := out + prompt
	if t < 1 {
		t = 1
	}
	return t
}

func plannerFleetFromRows(rows []FleetRateRow, modelRef string, itemsPerSec func(tps float32) float64) []PlannerWorker {
	out := make([]PlannerWorker, 0, len(rows))
	for _, r := range rows {
		if r.TPS <= 0 || r.Throttled {
			continue
		}
		cold := plannerDefaultColdLoadSecs
		if r.LoadMS > 0 {
			cold = float64(r.LoadMS) / 1000.0 // real measured load_ms beats the default
		}
		out = append(out, PlannerWorker{
			ID:           r.WorkerID,
			ItemsPerSec:  itemsPerSec(r.TPS),
			Warm:         modelRef == "" || r.Warm,
			ColdLoadSecs: cold,
		})
	}
	return out
}
