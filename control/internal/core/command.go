package core

import "github.com/google/uuid"

// Operation is one of the kernel's canonical exchange commands. Every external
// surface (native API, OpenAI Batch, Concierge, admin, worker protocol, proof)
// translates into one of these; the domain never learns which surface initiated it.
type Operation int

const (
	OpQuote Operation = iota
	OpSubmit
	OpInspect
	OpCancel
	OpClaim
	OpHeartbeat
	OpComplete
	OpFail
	OpRetry
	OpFinalize
	OpVerify
	OpCharge
	OpSettle
	OpRelease
	OpRefund
	OpDispute
	OpDeliverWebhook
	OpEnroll
)

func (o Operation) String() string {
	switch o {
	case OpQuote:
		return "Quote"
	case OpSubmit:
		return "Submit"
	case OpInspect:
		return "Inspect"
	case OpCancel:
		return "Cancel"
	case OpClaim:
		return "Claim"
	case OpHeartbeat:
		return "Heartbeat"
	case OpComplete:
		return "Complete"
	case OpFail:
		return "Fail"
	case OpRetry:
		return "Retry"
	case OpFinalize:
		return "Finalize"
	case OpVerify:
		return "Verify"
	case OpCharge:
		return "Charge"
	case OpSettle:
		return "Settle"
	case OpRelease:
		return "Release"
	case OpRefund:
		return "Refund"
	case OpDispute:
		return "Dispute"
	case OpDeliverWebhook:
		return "DeliverWebhook"
	case OpEnroll:
		return "Enroll"
	default:
		return "Unknown"
	}
}

// ActorKind is which credential plane authenticated a command. Authorization is a
// kernel stage, never a per-handler cast: a command carries its authenticated Actor
// and the pipeline enforces the operation's required plane + tenancy.
type ActorKind int

const (
	ActorBuyer ActorKind = iota
	ActorWorker
	ActorAdmin
	ActorSystem // background sweeps (retry, requeue, settlement, webhook delivery)
)

// Actor is the authenticated principal. Exactly the fields the plane needs are set;
// the buyer id is the tenancy fence for every buyer-scoped read/write.
type Actor struct {
	Kind       ActorKind
	BuyerID    uuid.UUID
	SupplierID uuid.UUID
	WorkerID   uuid.UUID
}

// Command is the single canonical internal request. Adapters fill only the fields
// their operation needs; the pipeline validates, loads, transitions, persists, and
// projects. Kept deliberately flat and typed (no reflection, no stringly options).
type Command struct {
	Actor       Actor
	Op          Operation
	JobID       uuid.UUID
	TaskID      uuid.UUID
	Workload    string // wire job_type; resolved through the Registry
	Idempotency string // caller idempotency key where the operation supports replay
	// Payload carries the operation-specific typed request (jobSubmit, claimRequest,
	// commitRequest, chargeRequest, ...). The pipeline type-switches on Op to the
	// concrete handler; adapters never reach past the kernel into the store.
	Payload any
}

// Fault is the kernel's projected failure: a stable code plus the default public
// status and message. Adapters may override only the wire message/status when a
// compatibility envelope requires it (e.g. the OpenAI error shape); the code and
// audit category stay canonical. A nil *Fault means success.
type Fault struct {
	Code      string // stable machine code (e.g. "not_found", "quote_expired", "budget_exceeded")
	Status    int    // default HTTP status
	Message   string // default public, IDOR-safe message
	Retryable bool   // whether a caller may safely retry
}

func (f *Fault) Error() string {
	if f == nil {
		return ""
	}
	return f.Message
}
