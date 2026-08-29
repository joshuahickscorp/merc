package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Catalogue publication vs the advertised buyer surface.
//
// BuildCataloguePriceSchedule / ApplyRepricing can (and on the staging plane
// did) publish an r6 schedule while GET /pricing/board.json still answers 503.
// handlePriceBoardData → loadCurrentPublicCatalogue walks activation.advertised,
// not the models table. An empty advertised set is therefore a published
// catalogue that cannot be shown.
//
// CapabilityDigest deliberately excludes benchmark_authority, so resealing a
// cell from r4 to r6 keeps the same digest. syncActivationPolicy will not
// overwrite an existing document seed. The r4 seed is then applied and
// storedRoutableEntryHasCurrentGlobalAuthority refuses it (promotion_receipt
// no longer equals the document). The previous reader treated that as
// QUARANTINE, which emptied advertised and 503'd the board.
//
// A drifted document seed is the same class of staleness as a digest mismatch:
// drop it and fall back to the current document. Operator and non-document
// rollback rows still quarantine — that is the gate-v4 rule this must not
// loosen.

// documentSourcedActivationFallsBackToDocument reports whether a stored
// statement that is no longer exact current global authority should be dropped
// (document fallback) rather than written into the overlay as QUARANTINED.
func documentSourcedActivationFallsBackToDocument(entry ActivationPolicyEntry) bool {
	if entry.Source == activationSourceDocument {
		return true
	}
	return entry.Source == activationSourceRollback &&
		entry.RestoredSource == activationSourceDocument
}

// documentActivationSeedDrifted reports that the latest stored statement is
// still labelled as a document seed but no longer equals the document. Those
// rows must be rewritten forward on migrate; operator rows are left alone.
func documentActivationSeedDrifted(stored, want ActivationPolicyEntry) bool {
	if stored.Source != activationSourceDocument {
		return false
	}
	return stored.ProfileRevision != want.ProfileRevision ||
		stored.CapabilityDigest != want.CapabilityDigest ||
		stored.Lifecycle != want.Lifecycle ||
		stored.PromotionReceipt != want.PromotionReceipt
}

// Activation policy: what a runtime is allowed to do today, as opposed to what it
// IS.
//
// The embedded document still declares a DEFAULT policy for each capability
// revision, because a newly registered profile has to start somewhere and the
// document is the only thing that exists before the first migration. After that
// the control plane owns it: a promotion is a policy write with a receipt, takes
// effect without editing the document, and — because the capability digest no
// longer moves — without rebuilding a single agent.
//
// The table is append-only and a rollback writes forward. An undo that erased the
// intervening revision would leave no record that the decision was ever taken,
// which is the opposite of what a rollback target is for.

// ActivationPolicyEntry is one governed statement about one profile or cell.
type ActivationPolicyEntry struct {
	PolicyRevision   int64  `json:"policy_revision"`
	RuntimeProfileID string `json:"runtime_profile_id"`
	ProfileRevision  string `json:"profile_revision"`
	// CellID is empty for the profile-level statement.
	CellID string `json:"cell_id"`
	// CapabilityDigest is the capability identity this statement was written
	// against. A policy whose digest no longer matches the document is stale and
	// is refused rather than applied — a capability change must not inherit the
	// activation decisions made about a different runtime.
	CapabilityDigest string `json:"capability_digest"`
	Lifecycle        string `json:"lifecycle"`
	Routable         bool   `json:"routable"`
	DirectedEligible bool   `json:"directed_eligible"`
	// CanaryAllowlist is the explicit cohort. nil means no cohort is defined; an
	// empty slice means a cohort with nobody in it, which is a different and
	// deliberate statement.
	CanaryAllowlist  []string  `json:"canary_allowlist,omitempty"`
	CanaryTrafficPct float64   `json:"canary_traffic_pct"`
	PromotionReceipt string    `json:"promotion_receipt"`
	RollbackTarget   *int64    `json:"rollback_target,omitempty"`
	EffectiveAt      time.Time `json:"effective_at"`
	Source           string    `json:"source"`
	Note             string    `json:"note,omitempty"`
	// RestoredSource is read-time provenance for a rollback row: the source of
	// the statement selected at RollbackTarget. It is deliberately not a wire or
	// storage field. A rollback must not launder an operator promotion accepted
	// under an obsolete gate into current authority merely because the new row's
	// source is "rollback".
	RestoredSource string `json:"-"`
}

const (
	activationSourceDocument = "document"
	activationSourceOperator = "operator"
	activationSourceRollback = "rollback"

	// Every activation-policy writer takes this transaction-scoped lock before
	// it reads the current epoch or allocates the next one. The policy revision
	// is a global epoch, not a per-cell counter: two disjoint writes sharing one
	// MAX(revision)+1 would make one revision mean two independently reviewed
	// decisions and would let a promotion validate against an epoch that changed
	// before it committed.
	activationPolicyAdvisoryLockID int64 = 0x4d45524341504359
)

// activationKey addresses a profile ("candle_metal") or one of its cells
// ("candle_metal/candle-metal-minilm-embed").
func activationKey(runtimeID, cellID string) string {
	if cellID == "" {
		return runtimeID
	}
	return runtimeID + "/" + cellID
}

// runtimeActivation is the snapshot the process is operating under: a lifecycle
// per profile and per cell, plus the two projections derived from it.
//
// Derived once and cached, because the projections are read on every quote,
// admission and enrolment and recomputing them per call would put document
// traversal on the hot path.
type runtimeActivation struct {
	PolicyRevision int64
	lifecycle      map[string]string
	advertised     []generatedRuntimeCapability
	directed       []generatedRuntimeCapability
	resolved       []authorityRuntimeProfile
	// Stale records policy rows that were refused because their capability digest
	// no longer matches the document. Surfaced rather than silently dropped: a
	// promotion that stopped applying is exactly the thing an operator must be
	// told about.
	Stale []string
}

// profileLifecycle resolves a profile's activation state.
func (a *runtimeActivation) profileLifecycle(p authorityRuntimeProfile) string {
	if state, ok := a.lifecycle[activationKey(p.RuntimeID, "")]; ok && state != "" {
		return state
	}
	return p.Lifecycle
}

// cellLifecycle resolves a cell's EFFECTIVE state under this policy: its own,
// floored by its profile's. Identical rule to the document's, applied to the
// policy's values — a profile still cannot inflate a cell and a cell still cannot
// outrank its profile.
func (a *runtimeActivation) cellLifecycle(p authorityRuntimeProfile, c authorityCell) string {
	own := c.Lifecycle
	if state, ok := a.lifecycle[activationKey(p.RuntimeID, c.ID)]; ok && state != "" {
		own = state
	}
	profileState := a.profileLifecycle(p)
	if own == "" {
		return profileState
	}
	ownRank, ownKnown := cellLifecycleRank(own)
	profileRank, profileKnown := cellLifecycleRank(profileState)
	if !ownKnown || !profileKnown || ownRank <= profileRank {
		return own
	}
	return profileState
}

func (a *runtimeActivation) cellRoutable(p authorityRuntimeProfile, c authorityCell) bool {
	// CANARY has no ordinary-routing consumer that enforces CanaryAllowlist or
	// CanaryTrafficPct. Until such a scoped admission path exists, advertising a
	// CANARY cell would send it unrestricted buyer traffic. It remains in the
	// directed set for governed evidence collection; only ACTIVE can enter the
	// ordinary advertised projection.
	if a.cellLifecycle(p, c) != runtimeLifecycleActive {
		return false
	}
	// Lifecycle comes from policy and authority binding from the document: even an
	// ACTIVE cell whose receipt was withdrawn is not routable, and an operator
	// write cannot make it so without new bindable authority.
	resolved := a.resolve(p)
	var cell authorityCell
	for _, candidate := range resolved.Cells {
		if candidate.ID == c.ID {
			cell = candidate
			break
		}
	}
	if cell.ID == "" {
		cell = c
		if state, ok := a.lifecycle[activationKey(p.RuntimeID, c.ID)]; ok && state != "" {
			cell.Lifecycle = state
		}
	}
	return cell.Routable(resolved)
}

func (a *runtimeActivation) cellDirected(p authorityRuntimeProfile, c authorityCell) bool {
	return directedReachableLifecycle(a.cellLifecycle(p, c))
}

// resolve returns a copy of the profile with policy lifecycles written into it.
//
// Resolving into the struct rather than teaching every reader about policy is
// what keeps the rest of the authority code pure: EffectiveLifecycle, Routable,
// ReachableByDirectedRouting and ValidateWorkerAgainstProfile all still answer
// questions about the profile value they were handed, so a test that hands them a
// QUARANTINED profile still gets the quarantine rule — which it did not when
// those functions consulted process-wide policy behind the caller's back.
func (a *runtimeActivation) resolve(profile authorityRuntimeProfile) authorityRuntimeProfile {
	if len(a.lifecycle) == 0 {
		return profile
	}
	resolved := profile
	resolved.Lifecycle = a.profileLifecycle(profile)
	resolved.Cells = append([]authorityCell(nil), profile.Cells...)
	for i, cell := range profile.Cells {
		if state, ok := a.lifecycle[activationKey(profile.RuntimeID, cell.ID)]; ok && state != "" {
			resolved.Cells[i].Lifecycle = state
		}
	}
	return resolved
}

// profiles returns every registered profile with policy applied.
//
// Precomputed with the projections. runtimeProfileByID is on the admission and
// scheduling path, and resolving allocates a cell slice per profile, so deriving
// this per call would put a few hundred bytes of garbage on every dispatch
// decision to answer a question whose answer only changes when policy does.
func (a *runtimeActivation) profiles() []authorityRuntimeProfile { return a.resolved }

func (a *runtimeActivation) profileByID(runtimeID string) (authorityRuntimeProfile, bool) {
	for _, profile := range a.resolved {
		if profile.RuntimeID == runtimeID {
			return profile, true
		}
	}
	return authorityRuntimeProfile{}, false
}

// routableProfiles returns the profiles allowed to receive buyer work under this
// policy.
func (a *runtimeActivation) routableProfiles() []authorityRuntimeProfile {
	out := make([]authorityRuntimeProfile, 0, len(runtimeAuthority.Runtimes))
	for _, profile := range a.profiles() {
		if runtimeLifecycleRoutable(profile.Lifecycle) {
			out = append(out, profile)
		}
	}
	return out
}

// newRuntimeActivation derives the projections once, from a lifecycle overlay.
func newRuntimeActivation(revision int64, overlay map[string]string, stale []string) *runtimeActivation {
	a := &runtimeActivation{PolicyRevision: revision, lifecycle: overlay, Stale: stale}
	a.advertised = projectCells(runtimeAuthority, a.cellRoutable)
	a.directed = projectCells(runtimeAuthority, a.cellDirected)
	a.resolved = make([]authorityRuntimeProfile, 0, len(runtimeAuthority.Runtimes))
	for _, profile := range runtimeAuthority.Runtimes {
		a.resolved = append(a.resolved, a.resolve(profile))
	}
	return a
}

// documentActivation is the default: exactly what the embedded document declares.
// It is what the process operates under before any policy has been loaded, and it
// is the seed for policy revision 1.
func documentActivation() *runtimeActivation {
	return newRuntimeActivation(0, map[string]string{}, nil)
}

var activeRuntimeActivation atomic.Pointer[runtimeActivation]

// The database advisory lock orders writers across processes. This local lock
// extends that ordering through RefreshActivationPolicy so an older writer's
// post-commit refresh cannot race a newer writer's refresh and regress the
// process-wide snapshot after the newer revision has already been installed.
var activationPolicyWriteMu sync.Mutex

// currentActivation is the policy every projection and admission decision reads.
func currentActivation() *runtimeActivation {
	if a := activeRuntimeActivation.Load(); a != nil {
		return a
	}
	// Fail to the document rather than to nothing. A control plane that could not
	// load policy must still refuse unproven runtimes, not admit everything.
	fallback := documentActivation()
	activeRuntimeActivation.CompareAndSwap(nil, fallback)
	return activeRuntimeActivation.Load()
}

// adoptActivation installs a policy for the whole process. Startup uses this
// unconditional form because it is binding the process to one database after a
// migration; live refreshes use adoptActivationIfNewer below.
func adoptActivation(a *runtimeActivation) { activeRuntimeActivation.Store(a) }

// adoptActivationIfNewer prevents an asynchronous/best-effort refresh from
// replacing a snapshot already known to include a later committed epoch. Policy
// is append-only, so an equal revision has equal meaning and need not replace
// the snapshot either.
func adoptActivationIfNewer(candidate *runtimeActivation) bool {
	if candidate == nil {
		return false
	}
	for {
		current := activeRuntimeActivation.Load()
		if current != nil && candidate.PolicyRevision <= current.PolicyRevision {
			return false
		}
		if activeRuntimeActivation.CompareAndSwap(current, candidate) {
			return true
		}
	}
}

// advertisedRuntimeCapabilities is the buyer-visible catalogue under the current
// policy.
func advertisedRuntimeCapabilities() []generatedRuntimeCapability {
	return currentActivation().advertised
}

// directedRuntimeCapabilities is the set an operator or a test may name. A
// superset of the advertised set, and not a catalogue.
func directedRuntimeCapabilities() []generatedRuntimeCapability {
	return currentActivation().directed
}

// documentActivationEntries is the policy the embedded document declares, used to
// seed a profile or cell that has never been given one.
func documentActivationEntries() ([]ActivationPolicyEntry, error) {
	var out []ActivationPolicyEntry
	for _, profile := range runtimeAuthority.Runtimes {
		digest, err := profile.CapabilityDigest(runtimeAuthorityModels)
		if err != nil {
			return nil, err
		}
		out = append(out, ActivationPolicyEntry{
			RuntimeProfileID: profile.RuntimeID,
			ProfileRevision:  profile.Revision,
			CapabilityDigest: digest,
			Lifecycle:        profile.Lifecycle,
			// Profile-level routable stays lifecycle-derived: the catalogue is
			// per cell. A profile with every cell unbound is still "ACTIVE" as a
			// document statement; its cells are simply not advertised.
			Routable:         profile.Lifecycle == runtimeLifecycleActive,
			DirectedEligible: directedReachableLifecycle(profile.Lifecycle),
			PromotionReceipt: profile.BenchmarkAuthority,
			Source:           activationSourceDocument,
		})
		for _, cell := range profile.Cells {
			effective := cell.EffectiveLifecycle(profile)
			out = append(out, ActivationPolicyEntry{
				RuntimeProfileID: profile.RuntimeID,
				ProfileRevision:  profile.Revision,
				CellID:           cell.ID,
				CapabilityDigest: digest,
				Lifecycle:        effective,
				// Cell routability is lifecycle AND bindable authority — the same
				// predicate projectActivationPolicyIntoRegistry writes into the
				// registry, so a document seed cannot advertise a cell the
				// predicate would refuse.
				Routable:         effective == runtimeLifecycleActive && cell.Routable(profile),
				DirectedEligible: directedReachableLifecycle(effective),
				PromotionReceipt: cell.benchmarkAuthorityFor(profile),
				Source:           activationSourceDocument,
			})
		}
	}
	return out, nil
}

// syncActivationPolicy seeds policy for any profile or cell that has none,
// and rewrites a document seed that no longer equals the current document.
//
// It does not overwrite an operator promotion. A sync that re-asserted the
// document on every migration would silently revert every decision an operator
// had made, which is the failure mode this table exists to prevent. A document
// seed whose promotion_receipt still names a superseded benchmark (r4 after an
// r6 reseal) is not an operator decision — CapabilityDigest excludes that
// path, so the row is not dropped as stale and would otherwise quarantine the
// advertised catalogue.
func syncActivationPolicy(ctx context.Context, tx pgx.Tx) error {
	entries, err := documentActivationEntries()
	if err != nil {
		return err
	}
	var toWrite []ActivationPolicyEntry
	var note string
	for _, entry := range entries {
		var stored ActivationPolicyEntry
		err := tx.QueryRow(ctx, `
			SELECT source, profile_revision, capability_digest, lifecycle, promotion_receipt
			  FROM runtime_activation_policies
			 WHERE runtime_profile_id=$1 AND cell_id=$2
			 ORDER BY policy_revision DESC
			 LIMIT 1`,
			entry.RuntimeProfileID, entry.CellID).Scan(
			&stored.Source, &stored.ProfileRevision, &stored.CapabilityDigest,
			&stored.Lifecycle, &stored.PromotionReceipt)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			toWrite = append(toWrite, entry)
			if note == "" {
				note = "seeded from the embedded capability document"
			}
		case err != nil:
			return fmt.Errorf("read activation policy for %s: %w",
				activationKey(entry.RuntimeProfileID, entry.CellID), err)
		case documentActivationSeedDrifted(stored, entry):
			toWrite = append(toWrite, entry)
			note = "refreshed document seed after benchmark-authority reseal"
		}
	}
	if len(toWrite) == 0 {
		return nil
	}
	if note == "" {
		note = "seeded from the embedded capability document"
	}
	_, err = insertActivationPolicy(ctx, tx, toWrite, activationSourceDocument, nil, note)
	return err
}

// insertActivationPolicy writes one new policy revision containing every entry.
func insertActivationPolicy(
	ctx context.Context, tx pgx.Tx, entries []ActivationPolicyEntry,
	source string, rollbackTarget *int64, note string,
) (int64, error) {
	if err := lockActivationPolicy(ctx, tx); err != nil {
		return 0, err
	}
	return insertActivationPolicyLocked(ctx, tx, entries, source, rollbackTarget, note)
}

// lockActivationPolicy serializes policy epoch reads and writes. Transaction
// scope is important: an error or caller cancellation releases the lock with
// the rollback, and a committed epoch becomes visible before the next writer
// validates its receipt.
func lockActivationPolicy(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, activationPolicyAdvisoryLockID); err != nil {
		return fmt.Errorf("lock activation policy epoch: %w", err)
	}
	return nil
}

// insertActivationPolicyLocked allocates and writes one revision while the
// caller holds activationPolicyAdvisoryLockID.
func insertActivationPolicyLocked(
	ctx context.Context, tx pgx.Tx, entries []ActivationPolicyEntry,
	source string, rollbackTarget *int64, note string,
) (int64, error) {
	if len(entries) == 0 {
		return 0, errors.New("an activation policy revision must contain at least one entry")
	}
	// A revision is one global epoch. Letting its rows become effective at
	// different instants changes policy semantics without changing MAX(revision),
	// which makes replica freshness impossible to prove. The database trigger
	// enforces this for every writer; reject it here too so the governed API gives
	// an actionable error instead of a trigger diagnostic.
	var (
		explicitEffectiveAt *time.Time
		usesDatabaseNow     bool
	)
	validationNow := time.Now().UTC()
	for _, entry := range entries {
		if entry.EffectiveAt.IsZero() {
			usesDatabaseNow = true
			continue
		}
		// Scheduled activation would let a future rN force every later revision
		// (including emergency quarantine or rollback) to wait behind it in order
		// to preserve monotonic epochs. Activation is therefore immediate or
		// historical only; future policy needs a separately governed scheduler
		// whose cancellation/containment semantics are explicit.
		if entry.EffectiveAt.After(validationNow) {
			return 0, errors.New("activation policy effective_at cannot be in the future")
		}
		if explicitEffectiveAt == nil {
			value := entry.EffectiveAt
			explicitEffectiveAt = &value
			continue
		}
		if !entry.EffectiveAt.Equal(*explicitEffectiveAt) {
			return 0, errors.New("all activation policy entries in one revision must have the same effective_at")
		}
	}
	if usesDatabaseNow && explicitEffectiveAt != nil {
		return 0, errors.New("activation policy revision cannot mix database-now and explicit effective_at values")
	}
	var next int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(policy_revision),0)+1 FROM runtime_activation_policies`).
		Scan(&next); err != nil {
		return 0, fmt.Errorf("allocate activation policy revision: %w", err)
	}
	for _, entry := range entries {
		var allowlist any
		if entry.CanaryAllowlist != nil {
			blob, err := json.Marshal(entry.CanaryAllowlist)
			if err != nil {
				return 0, err
			}
			allowlist = string(blob)
		}
		// NULL, not time.Now(). PostgreSQL's now() is the TRANSACTION start, so a
		// wall-clock timestamp taken in Go is always fractionally later than it —
		// and the "in force" read filters on effective_at <= clock_timestamp(). A policy
		// written and projected in one transaction would therefore have projected
		// nothing, silently, which is exactly how it first behaved.
		var effective any
		if !entry.EffectiveAt.IsZero() {
			effective = entry.EffectiveAt
		}
		// Routable is lifecycle-derived for the policy ROW only when the entry
		// does not carry a precomputed value — document seeds set it via
		// cell.Routable. Operator writes recompute against the live authority so
		// a withdrawn receipt cannot be re-advertised by lifecycle alone.
		routable := entry.Routable
		if entry.CellID != "" {
			if profile, ok := runtimeProfileByID(entry.RuntimeProfileID); ok {
				proposed := profile
				proposed.Cells = append([]authorityCell(nil), profile.Cells...)
				for j := range proposed.Cells {
					if proposed.Cells[j].ID == entry.CellID {
						proposed.Cells[j].Lifecycle = entry.Lifecycle
						routable = entry.Lifecycle == runtimeLifecycleActive &&
							proposed.Cells[j].Routable(proposed)
						break
					}
				}
			}
		} else {
			routable = entry.Lifecycle == runtimeLifecycleActive
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO runtime_activation_policies
			  (policy_revision, runtime_profile_id, profile_revision, cell_id,
			   capability_digest, lifecycle, routable, directed_eligible,
			   canary_allowlist, canary_traffic_pct, promotion_receipt,
			   rollback_target, effective_at, source, note)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,COALESCE($13,now()),$14,$15)`,
			next, entry.RuntimeProfileID, entry.ProfileRevision, entry.CellID,
			entry.CapabilityDigest, entry.Lifecycle,
			routable,
			directedReachableLifecycle(entry.Lifecycle),
			allowlist, entry.CanaryTrafficPct, entry.PromotionReceipt,
			rollbackTarget, effective, source, note); err != nil {
			return 0, fmt.Errorf("write activation policy for %s: %w",
				activationKey(entry.RuntimeProfileID, entry.CellID), err)
		}
	}
	return next, nil
}

// currentActivationEntries reads the newest effective statement for every
// profile and cell.
func currentActivationEntries(ctx context.Context, q pgxQuerier) ([]ActivationPolicyEntry, error) {
	rows, err := q.Query(ctx, `
		WITH current_policy AS (
			SELECT DISTINCT ON (runtime_profile_id, cell_id)
			       policy_revision, runtime_profile_id, profile_revision, cell_id,
			       capability_digest, lifecycle, routable, directed_eligible,
			       canary_allowlist, canary_traffic_pct, promotion_receipt,
			       rollback_target, effective_at, source, note
			  FROM runtime_activation_policies
			 WHERE effective_at <= clock_timestamp()
			 ORDER BY runtime_profile_id, cell_id, policy_revision DESC
		)
		SELECT c.policy_revision, c.runtime_profile_id, c.profile_revision, c.cell_id,
		       c.capability_digest, c.lifecycle, c.routable, c.directed_eligible,
		       c.canary_allowlist, c.canary_traffic_pct, c.promotion_receipt,
		       c.rollback_target, c.effective_at, c.source, c.note,
		       CASE WHEN c.source = 'rollback' AND c.rollback_target IS NOT NULL THEN
		         COALESCE((
		           SELECT target.source
		             FROM runtime_activation_policies target
		            WHERE target.runtime_profile_id = c.runtime_profile_id
		              AND target.cell_id = c.cell_id
		              AND target.policy_revision <= c.rollback_target
		            ORDER BY target.policy_revision DESC
		            LIMIT 1
		         ), '')
		       ELSE c.source END AS restored_source
		  FROM current_policy c
		 ORDER BY c.runtime_profile_id, c.cell_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActivationPolicyEntry
	for rows.Next() {
		var entry ActivationPolicyEntry
		var allowlist []byte
		if err := rows.Scan(
			&entry.PolicyRevision, &entry.RuntimeProfileID, &entry.ProfileRevision,
			&entry.CellID, &entry.CapabilityDigest, &entry.Lifecycle, &entry.Routable,
			&entry.DirectedEligible, &allowlist, &entry.CanaryTrafficPct,
			&entry.PromotionReceipt, &entry.RollbackTarget, &entry.EffectiveAt,
			&entry.Source, &entry.Note, &entry.RestoredSource); err != nil {
			return nil, err
		}
		if len(allowlist) > 0 {
			if err := json.Unmarshal(allowlist, &entry.CanaryAllowlist); err != nil {
				return nil, fmt.Errorf("decode canary allowlist for %s: %w",
					activationKey(entry.RuntimeProfileID, entry.CellID), err)
			}
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// pgxQuerier is the read surface shared by *pgxpool.Pool and pgx.Tx, so policy
// can be read inside the migration transaction that just wrote it as well as
// from a live pool.
type pgxQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// activationSnapshotFrom turns stored policy into the overlay the process runs
// under, refusing any statement whose capability identity no longer matches.
//
// A stale statement is dropped, not applied and not fatal. The profile falls back
// to what the document declares, which for a changed capability is the only thing
// that is actually known about it — the promotion was granted to a runtime that no
// longer exists in that form. Dropping it silently would be the dangerous half, so
// every drop is recorded.
func activationSnapshotFrom(entries []ActivationPolicyEntry) (*runtimeActivation, error) {
	digests := make(map[string]string, len(runtimeAuthority.Runtimes))
	revisions := make(map[string]string, len(runtimeAuthority.Runtimes))
	documentEntries, err := documentActivationEntries()
	if err != nil {
		return nil, err
	}
	documentByKey := make(map[string]ActivationPolicyEntry, len(documentEntries))
	for _, entry := range documentEntries {
		documentByKey[activationKey(entry.RuntimeProfileID, entry.CellID)] = entry
	}
	for _, profile := range runtimeAuthority.Runtimes {
		digest, err := profile.CapabilityDigest(runtimeAuthorityModels)
		if err != nil {
			return nil, err
		}
		digests[profile.RuntimeID] = digest
		revisions[profile.RuntimeID] = profile.Revision
	}
	overlay := make(map[string]string, len(entries))
	var stale []string
	var maxRevision int64
	for _, entry := range entries {
		if entry.PolicyRevision > maxRevision {
			maxRevision = entry.PolicyRevision
		}
		key := activationKey(entry.RuntimeProfileID, entry.CellID)
		want, known := digests[entry.RuntimeProfileID]
		switch {
		case !known:
			stale = append(stale, fmt.Sprintf(
				"%s: policy r%d names a profile the document no longer registers",
				key, entry.PolicyRevision))
			continue
		case entry.ProfileRevision != revisions[entry.RuntimeProfileID]:
			stale = append(stale, fmt.Sprintf(
				"%s: policy r%d was written for profile revision %s, document is at %s",
				key, entry.PolicyRevision, entry.ProfileRevision,
				revisions[entry.RuntimeProfileID]))
			continue
		case entry.CapabilityDigest != want:
			stale = append(stale, fmt.Sprintf(
				"%s: policy r%d was written against capability %s, document resolves %s",
				key, entry.PolicyRevision, entry.CapabilityDigest[:12], want[:12]))
			continue
		}
		lifecycle := entry.Lifecycle
		if runtimeLifecycleRoutable(lifecycle) &&
			!storedRoutableEntryHasCurrentGlobalAuthority(entry, documentByKey[key]) {
			// A document seed (or a rollback that restored one) whose
			// promotion_receipt no longer equals the document is stale in the
			// same way a digest mismatch is stale. Quarantining it empties the
			// advertised set and 503s GET /pricing/board.json even when the
			// current document is ACTIVE+BOUND and a schedule is published.
			// Operator rows still quarantine: that is the gate-v4 rule.
			if documentSourcedActivationFallsBackToDocument(entry) {
				stale = append(stale, fmt.Sprintf(
					"%s: policy r%d document seed no longer equals the current document; falling back to document",
					key, entry.PolicyRevision))
				continue
			}
			lifecycle = runtimeLifecycleQuarantined
			stale = append(stale, fmt.Sprintf(
				"%s: policy r%d %s %s statement has no current global authority; effective lifecycle is QUARANTINED",
				key, entry.PolicyRevision, entry.Source, entry.Lifecycle))
		}
		overlay[key] = lifecycle
	}
	sort.Strings(stale)
	return newRuntimeActivation(maxRevision, overlay, stale), nil
}

// storedRoutableEntryHasCurrentGlobalAuthority is the load/rollback boundary
// for CANARY and ACTIVE statements.
//
// Gate v4 intentionally cannot authorize a cell-global lifecycle: its evidence
// type covers one exact cohort and lacks durable matched execution/input-pair
// authority. Consequently, an operator statement accepted by gate v2 or by the
// old free-form receipt path is never grandfathered. Only the exact statement
// seeded from the current document is usable. A rollback may restore that
// statement when its target row was document-sourced; it may not turn an old
// operator row into authority merely by changing source to "rollback".
func storedRoutableEntryHasCurrentGlobalAuthority(
	entry, document ActivationPolicyEntry,
) bool {
	provenanceAuthorized := entry.Source == activationSourceDocument ||
		(entry.Source == activationSourceRollback &&
			entry.RestoredSource == activationSourceDocument)
	if !provenanceAuthorized {
		return false
	}
	if document.RuntimeProfileID == "" ||
		entry.RuntimeProfileID != document.RuntimeProfileID ||
		entry.CellID != document.CellID ||
		entry.ProfileRevision != document.ProfileRevision ||
		entry.CapabilityDigest != document.CapabilityDigest ||
		entry.Lifecycle != document.Lifecycle ||
		entry.PromotionReceipt != document.PromotionReceipt {
		return false
	}
	// ACTIVE is the only lifecycle admitted to ordinary routing. For a cell,
	// the current document must additionally bind the complete cell authority;
	// a historical document receipt that is now withdrawn cannot be revived.
	if entry.Lifecycle == runtimeLifecycleActive && entry.CellID != "" {
		return document.Routable
	}
	if entry.Lifecycle == runtimeLifecycleActive {
		for _, profile := range runtimeAuthority.Runtimes {
			if profile.RuntimeID != entry.RuntimeProfileID {
				continue
			}
			for _, cell := range profile.Cells {
				if cell.EffectiveLifecycle(profile) == runtimeLifecycleActive &&
					cell.Routable(profile) {
					return true
				}
			}
			return false
		}
		return false
	}
	return true
}

func readActivationSnapshot(
	ctx context.Context, q pgxQuerier,
) (*runtimeActivation, error) {
	entries, err := currentActivationEntries(ctx, q)
	if err != nil {
		return nil, err
	}
	return activationSnapshotFrom(entries)
}

// activationPolicyBestEffortRefresh is a test seam for the non-authoritative
// read after commit. Correctness never depends on this read: the exact snapshot
// was derived in the transaction and is installed first. A successful read can
// only advance the cache to an even newer cross-process commit.
var activationPolicyBestEffortRefresh = func(
	ctx context.Context, pool *pgxpool.Pool,
) (*runtimeActivation, error) {
	return readActivationSnapshot(ctx, pool)
}

var (
	errActivationAdmissionUnavailable = errors.New("runtime activation authority is unavailable for new admission")
	errActivationAdmissionStale       = errors.New("runtime activation authority changed during admission")
)

// activationForNewAdmission is the replica-freshness boundary for anything
// that is about to mint new runtime authority (a quote, an accepted job, or a
// worker registration).
//
// The process snapshot is deliberately a cache: another control process can
// commit a containment epoch without touching this process's atomic pointer.
// Therefore every new admission reads the database's complete in-force
// snapshot, compares its epoch with the cache, and advances the cache when it is
// behind. A read failure is a refusal, never permission to keep using the last
// snapshot. Historical reads and scheduling of already accepted work do not
// call this function; their authority is frozen on the durable object.
//
// This read is paired with guardActivationAdmissionTx at the eventual durable
// write. The pair closes the otherwise unavoidable interval in which another
// replica could commit quarantine after this read but before the admission row
// was inserted.
func (s *Store) activationForNewAdmission(
	ctx context.Context,
) (*runtimeActivation, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("%w: activation store is not configured",
			errActivationAdmissionUnavailable)
	}
	databaseSnapshot, err := readActivationSnapshot(ctx, s.pool)
	if err != nil {
		return nil, fmt.Errorf("%w: read current policy epoch: %v",
			errActivationAdmissionUnavailable, err)
	}
	cached := currentActivation()
	switch {
	case cached.PolicyRevision < databaseSnapshot.PolicyRevision:
		adoptActivationIfNewer(databaseSnapshot)
	case cached.PolicyRevision > databaseSnapshot.PolicyRevision:
		// In production every Store in a process points at the same database, so
		// an ahead cache means its provenance cannot be established for this
		// Store. Refuse instead of silently regressing the process-wide pointer.
		return nil, fmt.Errorf(
			"%w: cached policy epoch %d is ahead of database epoch %d",
			errActivationAdmissionUnavailable, cached.PolicyRevision,
			databaseSnapshot.PolicyRevision)
	}
	installed := currentActivation()
	if installed.PolicyRevision != databaseSnapshot.PolicyRevision {
		// A same-process policy write advanced the pointer between the read and
		// install. Retrying is safe; classifying against a potentially mixed pair
		// of epochs is not.
		return nil, fmt.Errorf(
			"%w: policy epoch advanced from %d to %d while refreshing",
			errActivationAdmissionStale, databaseSnapshot.PolicyRevision,
			installed.PolicyRevision)
	}
	return installed, nil
}

// activationAdmissionRevision returns the explicit epoch captured at request
// ingress, or the process snapshot for internal/test callers predating that
// field. The transaction guard below still compares that fallback with the
// database and fails closed if it is stale.
func activationAdmissionRevision(captured int64) int64 {
	if captured > 0 {
		return captured
	}
	return currentActivation().PolicyRevision
}

// guardActivationAdmissionTx closes the freshness/read-to-write race.
//
// Policy writers take the exclusive form of activationPolicyAdvisoryLockID.
// Admissions take the shared form only for their short durable transaction,
// then compare the exact epoch used to classify the request with PostgreSQL's
// current effective epoch. Consequently a quarantine either commits first and
// this transaction refuses without writes, or waits until this already-current
// admission commits. There is no ordering in which a stale admission can land
// after a committed quarantine.
func guardActivationAdmissionTx(
	ctx context.Context, tx pgx.Tx, expectedRevision int64,
) error {
	if expectedRevision <= 0 {
		return fmt.Errorf("%w: admission did not capture a positive policy epoch",
			errActivationAdmissionUnavailable)
	}
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock_shared($1)`,
		activationPolicyAdvisoryLockID,
	); err != nil {
		return fmt.Errorf("%w: lock current policy epoch: %v",
			errActivationAdmissionUnavailable, err)
	}
	var databaseRevision int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(policy_revision),0)
		  FROM runtime_activation_policies
		 WHERE effective_at <= clock_timestamp()`).Scan(&databaseRevision); err != nil {
		return fmt.Errorf("%w: read locked current policy epoch: %v",
			errActivationAdmissionUnavailable, err)
	}
	if databaseRevision != expectedRevision {
		return fmt.Errorf("%w: classified at epoch %d, database is at epoch %d",
			errActivationAdmissionStale, expectedRevision, databaseRevision)
	}
	return nil
}

// RefreshActivationPolicy reloads policy from PostgreSQL and installs it.
func (s *Store) RefreshActivationPolicy(ctx context.Context) (*runtimeActivation, error) {
	snapshot, err := readActivationSnapshot(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	adoptActivationIfNewer(snapshot)
	return snapshot, nil
}

// CurrentActivationPolicy is the read API: what is in force right now.
func (s *Store) CurrentActivationPolicy(ctx context.Context) ([]ActivationPolicyEntry, error) {
	return currentActivationEntries(ctx, s.pool)
}

// ApplyActivationPolicy writes a new revision and installs it.
//
// Every entry must name the capability digest the document currently resolves for
// that profile. Writing a promotion against a stale digest would produce a policy
// that is refused the moment it is read, so it is refused at the moment it is
// written instead, where the operator is still there to be told.
//
// Operator promotions into CANARY/ACTIVE require the cell-promotion-gate verdict
// as promotion_receipt — not a free path string. The gate's ReceiptRef is the
// only string this column may store for an operator promotion; a loose receipt
// path used to activate a cell without the gate ever having run.
func (s *Store) ApplyActivationPolicy(
	ctx context.Context, entries []ActivationPolicyEntry, note string,
) (int64, error) {
	return s.writeActivationPolicy(ctx, entries, activationSourceOperator, nil, note)
}

func (s *Store) writeActivationPolicy(
	ctx context.Context, entries []ActivationPolicyEntry,
	source string, rollbackTarget *int64, note string,
) (int64, error) {
	activationPolicyWriteMu.Lock()
	defer activationPolicyWriteMu.Unlock()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if err := lockActivationPolicy(ctx, tx); err != nil {
		return 0, err
	}

	// Read the activation snapshot only after taking the epoch lock. Receipt
	// validation and incumbent resolution must describe the same state the write
	// follows; consulting the process cache here would permit another committed
	// policy revision to sit between validation and insertion.
	lockedEntries, err := currentActivationEntries(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("read locked activation policy epoch: %w", err)
	}
	lockedActivation, err := activationSnapshotFrom(lockedEntries)
	if err != nil {
		return 0, fmt.Errorf("resolve locked activation policy epoch: %w", err)
	}

	for i, entry := range entries {
		profile, known := lockedActivation.profileByID(entry.RuntimeProfileID)
		if !known {
			return 0, fmt.Errorf("activation policy names unregistered profile %q",
				entry.RuntimeProfileID)
		}
		digest, err := profile.CapabilityDigest(runtimeAuthorityModels)
		if err != nil {
			return 0, err
		}
		if entry.CapabilityDigest != "" && entry.CapabilityDigest != digest {
			return 0, fmt.Errorf(
				"activation policy for %s names capability %s but %s %s resolves %s",
				activationKey(entry.RuntimeProfileID, entry.CellID),
				entry.CapabilityDigest, profile.RuntimeID, profile.Revision, digest)
		}
		entries[i].CapabilityDigest = digest
		entries[i].ProfileRevision = profile.Revision
		// A rollback writes forward, but it does not mint new authority. If the
		// target's routable statement came from an operator epoch (including legacy
		// gate-v2/free-form/profile writes), persist an explicit quarantine instead
		// of copying CANARY/ACTIVE into the new epoch. A current exact document row
		// remains restorable.
		if source == activationSourceRollback &&
			runtimeLifecycleRoutable(entry.Lifecycle) {
			candidate := entries[i]
			candidate.Source = activationSourceRollback
			documentEntries, docErr := documentActivationEntries()
			if docErr != nil {
				return 0, docErr
			}
			var documentEntry ActivationPolicyEntry
			for _, declared := range documentEntries {
				if activationKey(declared.RuntimeProfileID, declared.CellID) ==
					activationKey(candidate.RuntimeProfileID, candidate.CellID) {
					documentEntry = declared
					break
				}
			}
			if !storedRoutableEntryHasCurrentGlobalAuthority(candidate, documentEntry) {
				entries[i].Lifecycle = runtimeLifecycleQuarantined
				entries[i].Routable = false
				entries[i].DirectedEligible = false
				entry = entries[i]
			}
		}
		if _, known := cellLifecycleRank(entry.Lifecycle); !known {
			return 0, fmt.Errorf("activation policy for %s names unknown lifecycle %q",
				activationKey(entry.RuntimeProfileID, entry.CellID), entry.Lifecycle)
		}
		if entry.CellID != "" {
			var cell authorityCell
			found := false
			for _, candidate := range profile.Cells {
				if candidate.ID == entry.CellID {
					cell, found = candidate, true
					break
				}
			}
			if !found {
				return 0, fmt.Errorf("activation policy names cell %q, which profile %q %s "+
					"does not declare", entry.CellID, profile.RuntimeID, profile.Revision)
			}
			// A rejection is a measurement, and policy does not reverse measurements.
			//
			// Without this, an operator could promote llama.cpp's byte_exact generation
			// cell straight to CANARY by writing policy — the very cell whose
			// determinism sweep found divergence from its own serial output in every
			// batched configuration. Reversing that needs a new engine version, which
			// is a capability change with a new digest, not a policy write.
			if cell.Lifecycle == runtimeLifecycleRejectedForContract &&
				entry.Lifecycle != runtimeLifecycleRejectedForContract {
				return 0, fmt.Errorf(
					"cell %q is REJECTED_FOR_CONTRACT: %s\n"+
						"activation policy cannot reverse a measurement; that needs a new "+
						"capability revision", cell.ID, cell.RejectionReason)
			}
			// Every document-level cell rule, re-applied to the state policy proposes.
			//
			// The rules that decide whether a cell may sell its verification contract
			// live in validateCellAuthority and run when the document loads. Policy is
			// a second door into the same decision, so it has to face the same rules —
			// otherwise the determinism gate, the per-cell evidence requirement and the
			// receipt-binding check are all reachable around rather than through.
			proposed := profile
			proposed.Cells = append([]authorityCell(nil), profile.Cells...)
			for j := range proposed.Cells {
				if proposed.Cells[j].ID == cell.ID {
					// Only the lifecycle. The promotion receipt is the record of the
					// DECISION and the benchmark authority is the record of the
					// MEASUREMENT; substituting one for the other here would let a
					// promotion supply its own evidence.
					proposed.Cells[j].Lifecycle = entry.Lifecycle
					cell = proposed.Cells[j]
				}
			}
			if err := validateCellAuthority(proposed, cell); err != nil {
				return 0, fmt.Errorf("activation policy for %s: %w",
					activationKey(entry.RuntimeProfileID, entry.CellID), err)
			}
		}
		// Operator promotion into the routable lifecycle band requires authority
		// whose coverage is at least as wide as the lifecycle statement. Runs after
		// rejection/document rules so a measured rejection is still refused for
		// that reason, not for a missing gate receipt. Document seed and rollback
		// re-state prior receipts and are not promotions.
		if source == activationSourceOperator &&
			runtimeLifecycleRoutable(entry.Lifecycle) {
			if entry.CellID == "" {
				return 0, fmt.Errorf(
					"activation policy for profile %s promotes to %s globally, but %s receipts are exact cell/traffic/hardware scopes and cannot authorize a profile-global lifecycle",
					entry.RuntimeProfileID, entry.Lifecycle, promotionGateVersion)
			}
			if err := s.requirePromotionGateVerdict(
				ctx, tx, lockedActivation, entries[i],
			); err != nil {
				return 0, err
			}
		}
	}
	revision, err := insertActivationPolicyLocked(ctx, tx, entries, source, rollbackTarget, note)
	if err != nil {
		return 0, err
	}
	if err := projectActivationPolicyIntoRegistry(ctx, tx); err != nil {
		return 0, err
	}
	// Derive the exact in-force snapshot while the epoch lock is held and before
	// commit. This read sees the policy and registry projection as one atomic
	// decision; installing it cannot fail after commit.
	committedSnapshot, err := readActivationSnapshot(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("resolve committed activation policy epoch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	// Containment is effective in memory immediately after the durable commit.
	// The optional read can discover a newer cross-process epoch, but a failure is
	// non-authoritative and an older/equal snapshot cannot regress this one.
	adoptActivationIfNewer(committedSnapshot)
	if newest, refreshErr := activationPolicyBestEffortRefresh(ctx, s.pool); refreshErr == nil {
		adoptActivationIfNewer(newest)
	}
	return revision, nil
}

// requirePromotionGateVerdict authenticates and semantically checks the complete
// stored gate receipt against the locked activation epoch.
//
// Authentication is intentionally followed by a coverage refusal. A
// CellPromotionEvidence scope names one workload/model/tier/hardware/latency
// cohort, while ActivationPolicyEntry changes a cell's lifecycle for every
// cohort. The current receipt type has no representation for complete cell
// coverage, so even a well-formed narrow pass cannot authorize this global
// policy write. Keeping all checks before that refusal ensures a stale or
// fabricated row is diagnosed as such and cannot become latent authority if a
// future policy gains a scope narrow enough to consume these receipts.
func (s *Store) requirePromotionGateVerdict(
	ctx context.Context, q pgxQuerier, activation *runtimeActivation,
	entry ActivationPolicyEntry,
) error {
	key := activationKey(entry.RuntimeProfileID, entry.CellID)
	ref := strings.TrimSpace(entry.PromotionReceipt)
	if ref == "" {
		return fmt.Errorf(
			"activation policy for %s promotes to %s without a promotion receipt",
			key, entry.Lifecycle)
	}
	prefix := promotionGateVersion + ":" + entry.CellID + ":"
	if !strings.HasPrefix(ref, prefix) {
		return fmt.Errorf(
			"activation policy for %s: promotion_receipt must be a %s verdict for cell %q, got %q",
			key, promotionGateVersion, entry.CellID, ref)
	}
	refDigest := strings.TrimPrefix(ref, prefix)
	if len(refDigest) != 64 {
		return fmt.Errorf(
			"activation policy for %s: promotion_receipt digest is not a sha256",
			key)
	}
	for _, r := range refDigest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf(
				"activation policy for %s: promotion_receipt digest is not lowercase hex",
				key)
		}
	}

	var (
		storedDigest, storedRef, storedGateVersion string
		storedScopeJSON, storedEvidenceJSON        []byte
		storedIncumbent, storedChallenger          string
		storedPassed                               bool
		storedPolicyRevision                       int64
		storedRuntimeMatrix                        string
		storedEvaluatedAt                          time.Time
	)
	err := q.QueryRow(ctx, `
		SELECT evidence_sha256, promotion_receipt_ref, gate_version, scope_json,
		       incumbent_cell, challenger_cell, passed, policy_revision,
		       runtime_matrix_sha256, evaluated_at, evidence_json
		  FROM runtime_cell_promotion_evaluations
		 WHERE promotion_receipt_ref = $1`, ref).Scan(
		&storedDigest, &storedRef, &storedGateVersion, &storedScopeJSON,
		&storedIncumbent, &storedChallenger, &storedPassed, &storedPolicyRevision,
		&storedRuntimeMatrix, &storedEvaluatedAt, &storedEvidenceJSON)
	if err != nil {
		return fmt.Errorf(
			"activation policy for %s: promotion_receipt %q is not a recorded gate verdict "+
				"(EvaluateCellPromotion must pass and be recorded first): %w",
			key, ref, err)
	}

	var evidence CellPromotionEvidence
	if err := decodeStrictJSONObject(storedEvidenceJSON, &evidence); err != nil {
		return fmt.Errorf("activation policy for %s: promotion receipt evidence is not strict CellPromotionEvidence JSON: %w", key, err)
	}
	var projectedScope CellPromotionScope
	if err := decodeStrictJSONObject(storedScopeJSON, &projectedScope); err != nil {
		return fmt.Errorf("activation policy for %s: promotion receipt scope projection is invalid: %w", key, err)
	}
	computedDigest, err := evidence.Digest()
	if err != nil {
		return fmt.Errorf("activation policy for %s: digest promotion evidence: %w", key, err)
	}
	computedRef, err := evidence.ReceiptRef()
	if err != nil {
		return fmt.Errorf("activation policy for %s: derive promotion receipt reference: %w", key, err)
	}
	if storedRef != ref || storedDigest != refDigest || computedDigest != refDigest || computedRef != ref {
		return fmt.Errorf(
			"activation policy for %s: promotion receipt cryptographic identity disagrees (ref digest=%s stored digest=%s computed digest=%s computed ref=%q)",
			key, refDigest, storedDigest, computedDigest, computedRef)
	}
	if storedGateVersion != promotionGateVersion || evidence.GateVersion != promotionGateVersion {
		return fmt.Errorf(
			"activation policy for %s: promotion receipt gate version is stale or inconsistent (stored=%q evidence=%q current=%q)",
			key, storedGateVersion, evidence.GateVersion, promotionGateVersion)
	}
	if projectedScope != evidence.Scope {
		return fmt.Errorf("activation policy for %s: promotion receipt scope projection disagrees with evidence", key)
	}
	if storedIncumbent != evidence.IncumbentCell ||
		storedChallenger != evidence.Scope.CellID ||
		storedChallenger != entry.CellID {
		return fmt.Errorf(
			"activation policy for %s: promotion receipt pair projection disagrees (stored %s->%s, evidence %s->%s)",
			key, storedIncumbent, storedChallenger, evidence.IncumbentCell,
			evidence.Scope.CellID)
	}
	if storedPassed != evidence.Passed() {
		return fmt.Errorf(
			"activation policy for %s: promotion receipt passed projection=%t but evidence passed=%t",
			key, storedPassed, evidence.Passed())
	}
	if !storedPassed {
		return fmt.Errorf(
			"activation policy for %s: promotion gate verdict %q did not pass",
			key, ref)
	}
	if !storedEvaluatedAt.Equal(evidence.EvaluatedAt) &&
		math.Abs(float64(storedEvaluatedAt.Sub(evidence.EvaluatedAt))) >= float64(time.Microsecond) {
		return fmt.Errorf("activation policy for %s: promotion evaluated_at projection disagrees with evidence", key)
	}
	if !evidence.EvaluatedAt.Equal(evidence.Scope.ObservedBefore) ||
		evidence.Scope.ObservedBefore.Sub(evidence.Scope.ObservedAfter) != supplierLiabilityObservationWindow {
		return fmt.Errorf("activation policy for %s: promotion receipt does not bind the gate's exact observation window", key)
	}
	if storedPolicyRevision != evidence.PolicyRevision ||
		evidence.PolicyRevision != evidence.Scope.PolicyRevision ||
		evidence.RollbackTargetRevision != activation.PolicyRevision ||
		evidence.PolicyRevision != activation.PolicyRevision {
		return fmt.Errorf(
			"activation policy for %s: promotion receipt policy epoch is stale or inconsistent (stored=%d evidence=%d scope=%d rollback=%d locked-current=%d)",
			key, storedPolicyRevision, evidence.PolicyRevision,
			evidence.Scope.PolicyRevision, evidence.RollbackTargetRevision,
			activation.PolicyRevision)
	}
	if storedRuntimeMatrix != evidence.RuntimeMatrixSHA256 ||
		evidence.RuntimeMatrixSHA256 != evidence.Scope.RuntimeMatrixSHA256 ||
		evidence.RuntimeMatrixSHA256 != generatedRuntimeMatrixSHA256 {
		return fmt.Errorf(
			"activation policy for %s: promotion receipt runtime matrix is stale or inconsistent (stored=%s evidence=%s scope=%s current=%s)",
			key, storedRuntimeMatrix, evidence.RuntimeMatrixSHA256,
			evidence.Scope.RuntimeMatrixSHA256, generatedRuntimeMatrixSHA256)
	}
	if evidence.Scope.SelectionPolicy != shadowSelectionPolicy {
		return fmt.Errorf(
			"activation policy for %s: promotion receipt selection policy %q is not current %q",
			key, evidence.Scope.SelectionPolicy, shadowSelectionPolicy)
	}
	if evidence.Scope.RuntimeID != entry.RuntimeProfileID {
		return fmt.Errorf(
			"activation policy for %s: promotion receipt runtime %q does not match policy runtime %q",
			key, evidence.Scope.RuntimeID, entry.RuntimeProfileID)
	}

	profile, known := activation.profileByID(entry.RuntimeProfileID)
	if !known {
		return fmt.Errorf("activation policy for %s: promotion runtime is no longer registered", key)
	}
	capabilityDigest, err := profile.CapabilityDigest(runtimeAuthorityModels)
	if err != nil {
		return fmt.Errorf("activation policy for %s: resolve current capability identity: %w", key, err)
	}
	if entry.ProfileRevision != profile.Revision || entry.CapabilityDigest != capabilityDigest {
		return fmt.Errorf(
			"activation policy for %s: promotion policy identity does not match current capability %s/%s/%s",
			key, profile.RuntimeID, profile.Revision, capabilityDigest)
	}

	// Cross-check the durable registry projection as well as the embedded
	// authority. A drifted registry row must not be repaired by trusting whichever
	// side happens to make the promotion pass.
	var dbRevision, dbDigest, dbJob, dbModel, dbVerification, dbQuality string
	var dbHardware bool
	if err := q.QueryRow(ctx, `
		SELECT p.revision, p.profile_digest, m.job_type, m.model_id,
		       m.verification, m.quality_tier,
		       EXISTS (SELECT 1 FROM runtime_profile_hardware h
		                WHERE h.runtime_profile_id=p.runtime_profile_id
		                  AND h.revision=p.revision AND h.platform=$3)
		  FROM runtime_profiles p
		  JOIN runtime_profile_models m
		    ON m.runtime_profile_id=p.runtime_profile_id AND m.revision=p.revision
		 WHERE p.is_current AND p.runtime_profile_id=$1 AND m.cell_id=$2`,
		entry.RuntimeProfileID, entry.CellID, evidence.Scope.HWClass).Scan(
		&dbRevision, &dbDigest, &dbJob, &dbModel, &dbVerification, &dbQuality,
		&dbHardware); err != nil {
		return fmt.Errorf("activation policy for %s: current runtime/cell registry identity unavailable: %w", key, err)
	}
	if dbRevision != profile.Revision || dbDigest != capabilityDigest ||
		dbJob != evidence.Scope.JobType || dbModel != evidence.Scope.ModelRef ||
		dbVerification != evidence.Scope.Verification ||
		dbQuality != evidence.Scope.QualityTier || !dbHardware {
		return fmt.Errorf("activation policy for %s: promotion scope disagrees with the current runtime/cell registry projection", key)
	}

	liabilityScope := supplierLiabilityScope{
		JobType: evidence.Scope.JobType, ModelRef: evidence.Scope.ModelRef,
		HWClass:                 evidence.Scope.HWClass,
		HardwareIdentity:        evidence.Scope.HardwareIdentity,
		Tier:                    evidence.Scope.Tier,
		Currency:                evidence.Scope.Currency,
		CatalogueScheduleSHA256: evidence.Scope.CatalogueScheduleSHA256,
		RuntimeMatrixSHA256:     evidence.Scope.RuntimeMatrixSHA256,
		ModelRevision:           evidence.Scope.ModelRevision,
		QualityTier:             evidence.Scope.QualityTier,
		Verification:            evidence.Scope.Verification,
		LatencyClass:            evidence.Scope.LatencyClass,
		SelectionPolicy:         evidence.Scope.SelectionPolicy,
		PolicyRevision:          evidence.Scope.PolicyRevision,
		ObservedAfter:           evidence.Scope.ObservedAfter,
		ObservedBefore:          evidence.Scope.ObservedBefore,
		IncumbentCell:           evidence.IncumbentCell,
		ChallengerCell:          evidence.Scope.CellID,
	}
	if err := liabilityScope.validate(); err != nil {
		return fmt.Errorf("activation policy for %s: invalid exact promotion scope: %w", key, err)
	}
	authorityRefusals := []string{}
	if err := verifyPromotionScopeAgainstAuthority(
		activation, evidence.Scope, evidence.IncumbentCell,
		func(format string, args ...any) {
			authorityRefusals = append(authorityRefusals, fmt.Sprintf(format, args...))
		},
	); err != nil {
		return fmt.Errorf("activation policy for %s: validate promotion scope authority: %w", key, err)
	}
	if len(authorityRefusals) != 0 {
		return fmt.Errorf("activation policy for %s: promotion scope is not current authority: %s",
			key, strings.Join(authorityRefusals, "; "))
	}

	incumbentRuntime := promotionRuntimeForCell(
		activation, evidence.Scope.JobType, evidence.Scope.ModelRef,
		evidence.IncumbentCell)
	validateProxyIdentity := func(
		label string, proxy MeasuredSupplierLiabilityProxy,
		wantCell, wantRuntime string,
	) error {
		if proxy.CellID != wantCell || proxy.RuntimeID != wantRuntime ||
			proxy.JobType != evidence.Scope.JobType ||
			proxy.ModelRef != evidence.Scope.ModelRef ||
			proxy.HWClass != evidence.Scope.HWClass ||
			proxy.HardwareIdentity != evidence.Scope.HardwareIdentity ||
			proxy.Currency != evidence.Scope.Currency ||
			proxy.RuntimeMatrixSHA256 != evidence.Scope.RuntimeMatrixSHA256 ||
			proxy.SourceBinding != BindingBound ||
			strings.TrimSpace(proxy.ExecutionBuildHash) == "" ||
			!validSHA256(proxy.InputGeometrySHA256) {
			return fmt.Errorf("%s measured proxy does not bind the exact cell/runtime/build/geometry scope", label)
		}
		if err := validateMeasuredProxyCurrentExecutionIdentity(proxy, wantCell); err != nil {
			return fmt.Errorf("%s measured proxy does not bind the current exact execution identity", label)
		}
		if _, ok := eligibleMeasuredSupplierLiability(proxy); !ok {
			return fmt.Errorf("%s measured proxy is not promotion-eligible", label)
		}
		return nil
	}
	if err := validateProxyIdentity("challenger", evidence.ChallengerSupplierLiability,
		evidence.Scope.CellID, evidence.Scope.RuntimeID); err != nil {
		return fmt.Errorf("activation policy for %s: %w", key, err)
	}
	if err := validateProxyIdentity("incumbent", evidence.IncumbentSupplierLiability,
		evidence.IncumbentCell, incumbentRuntime); err != nil {
		return fmt.Errorf("activation policy for %s: %w", key, err)
	}
	if evidence.ChallengerSupplierLiability.InputGeometrySHA256 !=
		evidence.IncumbentSupplierLiability.InputGeometrySHA256 {
		return fmt.Errorf("activation policy for %s: promotion proxies do not share exact input geometry", key)
	}
	challengerLiability, _ := eligibleMeasuredSupplierLiability(evidence.ChallengerSupplierLiability)
	incumbentLiability, _ := eligibleMeasuredSupplierLiability(evidence.IncumbentSupplierLiability)
	if evidence.ChallengerSupplierLiability.MedianMsPerUnit <= 0 ||
		evidence.IncumbentSupplierLiability.MedianMsPerUnit <= 0 {
		return fmt.Errorf("activation policy for %s: passed promotion evidence has no positive per-unit latency authority", key)
	}
	wantGain := (evidence.IncumbentSupplierLiability.MedianMsPerUnit -
		evidence.ChallengerSupplierLiability.MedianMsPerUnit) /
		evidence.IncumbentSupplierLiability.MedianMsPerUnit
	wantLatencyRatio := evidence.ChallengerSupplierLiability.MedianMsPerUnit /
		evidence.IncumbentSupplierLiability.MedianMsPerUnit
	if evidence.Basis != promotionBasisThroughput ||
		evidence.RequiredMarginFraction != promotionThroughputMarginFraction ||
		!supplierLiabilitiesTieUSD(challengerLiability, incumbentLiability) ||
		evidence.ThroughputGainFraction < promotionThroughputMarginFraction ||
		math.Abs(evidence.ThroughputGainFraction-wantGain) > 1e-12 ||
		math.Abs(evidence.LatencyRatio-wantLatencyRatio) > 1e-12 ||
		evidence.ChallengerSupplierLiabilityUSDPerVerifiedUnit != challengerLiability ||
		evidence.IncumbentSupplierLiabilityUSDPerVerifiedUnit != incumbentLiability ||
		evidence.LiabilityRegret.ExactPairScoredDecisions <= 0 ||
		evidence.LiabilityRegret.JobType != evidence.Scope.JobType ||
		evidence.LiabilityRegret.ModelRef != evidence.Scope.ModelRef ||
		evidence.LiabilityRegret.HWClass != evidence.Scope.HWClass ||
		evidence.LiabilityRegret.HardwareIdentity != evidence.Scope.HardwareIdentity ||
		evidence.LiabilityRegret.Currency != evidence.Scope.Currency ||
		!slices.Equal(evidence.UnknownPlatformCostComponents,
			unresolvedPlatformCostComponents(evidence.ChallengerSupplierLiability,
				evidence.IncumbentSupplierLiability)) {
		return fmt.Errorf("activation policy for %s: passed promotion evidence is not semantically consistent with the current gate", key)
	}

	return fmt.Errorf(
		"activation policy for %s: receipt %q cannot authorize promotion: %s; independently, it covers only exact scope job=%s model=%s tier=%s hardware=%s/%s latency=%s while cell lifecycle %s is global across traffic scopes and CellPromotionEvidence has no global-coverage authority",
		key, ref, promotionMatchedPairAuthorityRefusal,
		evidence.Scope.JobType, evidence.Scope.ModelRef,
		evidence.Scope.Tier, evidence.Scope.HWClass, evidence.Scope.HardwareIdentity,
		evidence.Scope.LatencyClass,
		entry.Lifecycle)
}

// RollbackActivationPolicy restores an earlier revision by writing it forward.
//
// The restored statements keep their own content and gain a rollback_target
// naming what they came from, so the history reads "we promoted, then we undid
// it" rather than "the promotion never happened".
func (s *Store) RollbackActivationPolicy(
	ctx context.Context, target int64, note string,
) (int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (runtime_profile_id, cell_id)
		       runtime_profile_id, profile_revision, cell_id, capability_digest,
		       lifecycle, canary_allowlist, canary_traffic_pct, promotion_receipt,
		       source
		  FROM runtime_activation_policies
		 WHERE policy_revision <= $1
		 ORDER BY runtime_profile_id, cell_id, policy_revision DESC`, target)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var entries []ActivationPolicyEntry
	for rows.Next() {
		var entry ActivationPolicyEntry
		var allowlist []byte
		if err := rows.Scan(&entry.RuntimeProfileID, &entry.ProfileRevision, &entry.CellID,
			&entry.CapabilityDigest, &entry.Lifecycle, &allowlist,
			&entry.CanaryTrafficPct, &entry.PromotionReceipt,
			&entry.RestoredSource); err != nil {
			return 0, err
		}
		if len(allowlist) > 0 {
			if err := json.Unmarshal(allowlist, &entry.CanaryAllowlist); err != nil {
				return 0, err
			}
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(entries) == 0 {
		return 0, fmt.Errorf("activation policy revision %d has nothing to roll back to", target)
	}
	return s.writeActivationPolicy(ctx, entries, activationSourceRollback, &target, note)
}

// projectActivationPolicyIntoRegistry pushes the in-force policy onto the
// denormalized lifecycle and routability columns the scheduler and admission
// queries read.
//
// Those columns exist because a PostgreSQL index predicate cannot contain a
// subquery, so they are a cache of the policy rather than a second source of
// truth. Writing them in the same transaction as the policy is what keeps them
// from being one.
func projectActivationPolicyIntoRegistry(ctx context.Context, tx pgx.Tx) error {
	entries, err := currentActivationEntries(ctx, tx)
	if err != nil {
		return err
	}
	snapshot, err := activationSnapshotFrom(entries)
	if err != nil {
		return err
	}
	for _, profile := range runtimeAuthority.Runtimes {
		state := snapshot.profileLifecycle(profile)
		if _, err := tx.Exec(ctx, `
			UPDATE runtime_profiles
			   SET lifecycle = $3,
			       routable = (is_current AND $3 = 'ACTIVE'),
			       updated_at = now()
			 WHERE runtime_profile_id = $1 AND revision = $2`,
			profile.RuntimeID, profile.Revision, state); err != nil {
			return fmt.Errorf("project activation policy onto %s %s: %w",
				profile.RuntimeID, profile.Revision, err)
		}
		for _, cell := range profile.Cells {
			effective := snapshot.cellLifecycle(profile, cell)
			// Registry routable tracks the complete ordinary-buyer predicate:
			// ACTIVE plus bindable authority. CANARY remains directed-only because
			// no admission path consumes its allowlist/percentage fields.
			routable := snapshot.cellRoutable(profile, cell)
			if _, err := tx.Exec(ctx, `
				UPDATE runtime_profile_models
				   SET lifecycle = $4, routable = $5
				 WHERE runtime_profile_id = $1 AND revision = $2 AND cell_id = $3`,
				profile.RuntimeID, profile.Revision, cell.ID, effective, routable); err != nil {
				return fmt.Errorf("project activation policy onto cell %s: %w", cell.ID, err)
			}
		}
	}
	return nil
}

// loadActivationAtStartup installs stored policy before the server serves
// anything. Called from Migrate, which every entry point runs.
func loadActivationAtStartup(ctx context.Context, pool *pgxpool.Pool) error {
	entries, err := currentActivationEntries(ctx, pool)
	if err != nil {
		return err
	}
	snapshot, err := activationSnapshotFrom(entries)
	if err != nil {
		return err
	}
	adoptActivation(snapshot)
	return nil
}
