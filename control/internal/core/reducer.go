package core

// reducer.go — the single authoritative state machine for the exchange's durable
// entities. The legacy tree scatters status transitions across ~129 UPDATE sites in
// handlers, store methods, workers, and tests; the kernel funnels every transition
// through Reduce so the legality of a move is defined once and consulted by HTTP,
// workers, retries, recovery, admin actions, proof, and tests alike. Illegal moves
// fail closed. The database still enforces money/concurrency invariants (locks,
// CHECKs, triggers); this reducer is the in-process authority on WHICH moves exist.

// Entity is a durable state-bearing thing in the exchange.
type Entity string

const (
	EntityJob          Entity = "job"
	EntityTask         Entity = "task"
	EntityPayout       Entity = "payout"       // ledger supplier_credit.payout_status
	EntityCharge       Entity = "charge"       // jobs.charge_status
	EntityVerification Entity = "verification" // verification_work.status
)

// transitions is the legal state graph per entity: from-state -> set of to-states.
// It is intentionally the WHOLE truth table for supported lifecycles, derived from
// the behavioral oracle. A transition absent here is illegal and Reduce refuses it.
var transitions = map[Entity]map[string]map[string]bool{
	EntityJob: {
		"queued":    set("running", "cancelled"),
		"running":   set("verifying", "complete", "failed", "cancelled"),
		"verifying": set("complete", "failed"),
		// terminal
		"complete":  {},
		"failed":    {},
		"cancelled": {},
	},
	EntityTask: {
		"queued":    set("leased", "cancelled"),
		"leased":    set("running", "queued", "failed"), // lease expiry requeues to queued
		"running":   set("verifying", "failed", "queued"),
		"verifying": set("done", "failed", "retrying"),
		"retrying":  set("queued"),
		// terminal
		"done":      {},
		"failed":    {},
		"cancelled": {},
	},
	EntityPayout: {
		"held":              set("sending", "released", "clawed_back", "reversal_required"),
		"sending":           set("released", "outcome_unknown", "reversal_required"),
		"outcome_unknown":   set("released", "reversal_required"),
		"released":          set("reversal_required"),
		"reversal_required": set("reversed"),
		// terminal
		"clawed_back": {},
		"reversed":    {},
	},
	EntityCharge: {
		"not_attempted": set("attempting", "deferred"),
		"attempting":    set("charged", "not_attempted", "deferred"),
		"deferred":      set("attempting"),
		// terminal
		"charged": {},
	},
	EntityVerification: {
		"pending":  set("leased", "terminal"),
		"leased":   set("terminal", "pending"), // fenced lease expiry returns to pending
		"terminal": {},
	},
}

func set(states ...string) map[string]bool {
	m := make(map[string]bool, len(states))
	for _, s := range states {
		m[s] = true
	}
	return m
}

// Legal reports whether entity may move from -> to. Unknown entity or unknown
// from-state is illegal (fail closed): the reducer never invents a lifecycle.
func Legal(entity Entity, from, to string) bool {
	byFrom, ok := transitions[entity]
	if !ok {
		return false
	}
	outs, ok := byFrom[from]
	if !ok {
		return false
	}
	return outs[to]
}

// Terminal reports whether a state has no outgoing transitions (a settled endpoint).
func Terminal(entity Entity, state string) bool {
	byFrom, ok := transitions[entity]
	if !ok {
		return false
	}
	outs, ok := byFrom[state]
	return ok && len(outs) == 0
}

// Reduce validates a transition and returns the target state or a Fault. Callers use
// it as the guard immediately before persisting a status change, so an illegal move
// is refused in-process before it can reach (and be rejected less legibly by) the DB.
func Reduce(entity Entity, from, to string) (string, *Fault) {
	if Legal(entity, from, to) {
		return to, nil
	}
	return "", &Fault{
		Code:      "illegal_transition",
		Status:    409,
		Message:   "illegal state transition",
		Retryable: false,
	}
}
