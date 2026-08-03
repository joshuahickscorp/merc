package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
}

const (
	activationSourceDocument = "document"
	activationSourceOperator = "operator"
	activationSourceRollback = "rollback"
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
	// Lifecycle from policy, authority binding from the document: a CANARY cell
	// whose receipt was withdrawn is not routable, and an operator write cannot
	// make it so without a new bindable authority.
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

// adoptActivation installs a refreshed policy for the whole process.
func adoptActivation(a *runtimeActivation) { activeRuntimeActivation.Store(a) }

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
			Routable:         runtimeLifecycleRoutable(profile.Lifecycle),
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
				Routable:         cell.Routable(profile),
				DirectedEligible: directedReachableLifecycle(effective),
				PromotionReceipt: cell.benchmarkAuthorityFor(profile),
				Source:           activationSourceDocument,
			})
		}
	}
	return out, nil
}

// syncActivationPolicy seeds policy for any profile or cell that has none.
//
// It seeds, it does not overwrite. An operator promotion recorded in the policy
// table must survive a redeploy of the control plane; a sync that re-asserted the
// document on every migration would silently revert every decision an operator
// had made, which is the failure mode this table exists to prevent.
func syncActivationPolicy(ctx context.Context, tx pgx.Tx) error {
	entries, err := documentActivationEntries()
	if err != nil {
		return err
	}
	var missing []ActivationPolicyEntry
	for _, entry := range entries {
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM runtime_activation_policies
			                WHERE runtime_profile_id=$1 AND profile_revision=$2 AND cell_id=$3)`,
			entry.RuntimeProfileID, entry.ProfileRevision, entry.CellID).Scan(&exists); err != nil {
			return fmt.Errorf("read activation policy for %s: %w",
				activationKey(entry.RuntimeProfileID, entry.CellID), err)
		}
		if !exists {
			missing = append(missing, entry)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	_, err = insertActivationPolicy(ctx, tx, missing, activationSourceDocument, nil,
		"seeded from the embedded capability document")
	return err
}

// insertActivationPolicy writes one new policy revision containing every entry.
func insertActivationPolicy(
	ctx context.Context, tx pgx.Tx, entries []ActivationPolicyEntry,
	source string, rollbackTarget *int64, note string,
) (int64, error) {
	if len(entries) == 0 {
		return 0, errors.New("an activation policy revision must contain at least one entry")
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
		// and the "in force" read filters on effective_at <= now(). A policy
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
						routable = proposed.Cells[j].Routable(proposed)
						break
					}
				}
			}
		} else {
			routable = runtimeLifecycleRoutable(entry.Lifecycle)
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
		SELECT DISTINCT ON (runtime_profile_id, cell_id)
		       policy_revision, runtime_profile_id, profile_revision, cell_id,
		       capability_digest, lifecycle, routable, directed_eligible,
		       canary_allowlist, canary_traffic_pct, promotion_receipt,
		       rollback_target, effective_at, source, note
		  FROM runtime_activation_policies
		 WHERE effective_at <= now()
		 ORDER BY runtime_profile_id, cell_id, policy_revision DESC`)
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
			&entry.Source, &entry.Note); err != nil {
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
		overlay[key] = entry.Lifecycle
		if entry.PolicyRevision > maxRevision {
			maxRevision = entry.PolicyRevision
		}
	}
	sort.Strings(stale)
	return newRuntimeActivation(maxRevision, overlay, stale), nil
}

// RefreshActivationPolicy reloads policy from PostgreSQL and installs it.
func (s *Store) RefreshActivationPolicy(ctx context.Context) (*runtimeActivation, error) {
	entries, err := currentActivationEntries(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	snapshot, err := activationSnapshotFrom(entries)
	if err != nil {
		return nil, err
	}
	adoptActivation(snapshot)
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
	for i, entry := range entries {
		profile, known := runtimeProfileByID(entry.RuntimeProfileID)
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
		// Operator promotion into the routable lifecycle band requires the gate's
		// verdict. Runs after rejection/document rules so a measured rejection is
		// still refused for that reason, not for a missing gate receipt. Document
		// seed and rollback re-state prior receipts and are not promotions.
		// Profile-level entries have no cell gate (promotion is per cell).
		if source == activationSourceOperator &&
			runtimeLifecycleRoutable(entry.Lifecycle) &&
			entry.CellID != "" {
			if err := s.requirePromotionGateVerdict(ctx, entry); err != nil {
				return 0, err
			}
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	revision, err := insertActivationPolicy(ctx, tx, entries, source, rollbackTarget, note)
	if err != nil {
		return 0, err
	}
	if err := projectActivationPolicyIntoRegistry(ctx, tx); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	if _, err := s.RefreshActivationPolicy(ctx); err != nil {
		return revision, err
	}
	return revision, nil
}

// requirePromotionGateVerdict refuses an operator promotion whose receipt is
// not a passed evaluation of the cell-promotion gate.
//
// The gate's ReceiptRef format is `cell-promotion-gate-v2:<cell_id>:<sha256>`.
// Anything else — a benchmark path, a chain receipt, a free string — used to
// satisfy the non-empty CHECK and activate the cell without the gate running.
func (s *Store) requirePromotionGateVerdict(ctx context.Context, entry ActivationPolicyEntry) error {
	ref := strings.TrimSpace(entry.PromotionReceipt)
	if ref == "" {
		return fmt.Errorf(
			"activation policy for %s promotes to %s without a promotion receipt",
			activationKey(entry.RuntimeProfileID, entry.CellID), entry.Lifecycle)
	}
	prefix := promotionGateVersion + ":" + entry.CellID + ":"
	if !strings.HasPrefix(ref, prefix) {
		return fmt.Errorf(
			"activation policy for %s: promotion_receipt must be a %s verdict for cell %q, got %q",
			activationKey(entry.RuntimeProfileID, entry.CellID), promotionGateVersion,
			entry.CellID, ref)
	}
	digest := strings.TrimPrefix(ref, prefix)
	if len(digest) != 64 {
		return fmt.Errorf(
			"activation policy for %s: promotion_receipt digest is not a sha256",
			activationKey(entry.RuntimeProfileID, entry.CellID))
	}
	for _, r := range digest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf(
				"activation policy for %s: promotion_receipt digest is not lowercase hex",
				activationKey(entry.RuntimeProfileID, entry.CellID))
		}
	}
	var passed bool
	err := s.pool.QueryRow(ctx, `
		SELECT passed FROM runtime_cell_promotion_evaluations
		 WHERE promotion_receipt_ref = $1`, ref).Scan(&passed)
	if err != nil {
		return fmt.Errorf(
			"activation policy for %s: promotion_receipt %q is not a recorded gate verdict "+
				"(EvaluateCellPromotion must pass and be recorded first): %w",
			activationKey(entry.RuntimeProfileID, entry.CellID), ref, err)
	}
	if !passed {
		return fmt.Errorf(
			"activation policy for %s: promotion gate verdict %q did not pass",
			activationKey(entry.RuntimeProfileID, entry.CellID), ref)
	}
	return nil
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
		       lifecycle, canary_allowlist, canary_traffic_pct, promotion_receipt
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
			&entry.CanaryTrafficPct, &entry.PromotionReceipt); err != nil {
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
			       routable = (is_current AND $3 IN ('CANARY','ACTIVE')),
			       updated_at = now()
			 WHERE runtime_profile_id = $1 AND revision = $2`,
			profile.RuntimeID, profile.Revision, state); err != nil {
			return fmt.Errorf("project activation policy onto %s %s: %w",
				profile.RuntimeID, profile.Revision, err)
		}
		for _, cell := range profile.Cells {
			effective := snapshot.cellLifecycle(profile, cell)
			// Registry routable tracks the full predicate (lifecycle + bindable
			// authority), not lifecycle alone. The DB CHECK allows CANARY/ACTIVE
			// with routable=false so a withdrawn receipt demotes without editing
			// the lifecycle field by hand.
			resolved := snapshot.resolve(profile)
			routable := false
			for _, candidate := range resolved.Cells {
				if candidate.ID == cell.ID {
					// Effective lifecycle is already on the resolved cell when
					// policy overlaid it; force the projected effective so a
					// policy demotion is visible even if resolve missed it.
					candidate.Lifecycle = effective
					routable = candidate.Routable(resolved)
					break
				}
			}
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
