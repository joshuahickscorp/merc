package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	serviceLeaseHeartbeatTimeout = 45 * time.Second
	// Current service policy rates are explicitly USD. A future non-USD service
	// authority must convert them through a frozen FX snapshot, not reuse these
	// numerics under another currency label.
	serviceLeaseControlUSDNanosHour = int64(100_000_000)
	serviceLeaseContributionUSDHour = int64(300_000_000)
)

var serviceLeaseRegionPattern = regexp.MustCompile(`^[a-z0-9-]{2,64}$`)

var errServiceLeaseOfferBelowReservations = errors.New("service lease offer maximum cannot fall below active reserved replicas")

type ServiceLeaseOfferRegistration struct {
	RuntimeProfileID             string `json:"runtime_profile_id"`
	RuntimeProfileSHA256         string `json:"runtime_profile_sha256"`
	Region                       string `json:"region"`
	Currency                     string `json:"currency"`
	MaximumWarmReplicas          int    `json:"maximum_warm_replicas"`
	AvailableWarmReplicas        int    `json:"available_warm_replicas"`
	SupplierNanosPerReplicaHour  int64  `json:"supplier_nanos_per_replica_hour"`
	ResidencyNanosPerReplicaHour int64  `json:"residency_nanos_per_replica_hour"`
	SupportsRollingUpgrade       bool   `json:"supports_rolling_upgrade"`
	P95LatencyMillis             int64  `json:"p95_latency_milliseconds"`
	LatencyMeasurementCount      int    `json:"latency_measurement_count"`
	LatencyWindowSeconds         int64  `json:"latency_window_seconds"`
	LatencyMeasurementKind       string `json:"latency_measurement_kind"`
	Status                       string `json:"status"`
}

type ServiceLeaseRequest struct {
	RuntimeProfileID              string `json:"runtime_profile_id"`
	Region                        string `json:"region"`
	Currency                      string `json:"currency"`
	MinimumReplicas               int    `json:"minimum_replicas"`
	MaximumReplicas               int    `json:"maximum_replicas"`
	TermSeconds                   int64  `json:"term_seconds"`
	MaximumP95LatencyMilliseconds int64  `json:"maximum_p95_latency_milliseconds"`
	BuyerDeclaredCeilingNanos     int64  `json:"buyer_declared_ceiling_nanos"`
}

type ServiceLeaseHeartbeat struct {
	WarmReplicas            int    `json:"warm_replicas"`
	P95LatencyMillis        int64  `json:"p95_latency_milliseconds"`
	LatencyMeasurementCount int    `json:"latency_measurement_count"`
	LatencyWindowSeconds    int64  `json:"latency_window_seconds"`
	LatencyMeasurementKind  string `json:"latency_measurement_kind"`
	// A READY heartbeat binds its aggregate latency to the exact bounded
	// request/response probe that produced it. This is worker-attested evidence,
	// not a buyer-request receipt or an independent availability proof.
	DataPlaneProbeReceiptSHA256 string `json:"data_plane_probe_receipt_sha256,omitempty"`
	Status                      string `json:"status"`
	UpgradeGeneration           string `json:"upgrade_generation,omitempty"`
}

// ServiceLeaseAssignment is the minimum lease authority a worker needs to
// operate a reserved service. It deliberately omits buyer identity, pricing,
// prompts, and payment facts; those remain buyer/control-plane concerns.
type ServiceLeaseAssignment struct {
	ID                      uuid.UUID `json:"id"`
	RuntimeProfileID        string    `json:"runtime_profile_id"`
	Region                  string    `json:"region"`
	MinimumReplicas         int       `json:"minimum_replicas"`
	MaximumReplicas         int       `json:"maximum_replicas"`
	MaximumP95LatencyMillis int64     `json:"maximum_p95_latency_milliseconds"`
	State                   string    `json:"state"`
	UpgradeGeneration       string    `json:"upgrade_generation,omitempty"`
	ExpiresAt               time.Time `json:"expires_at"`
}

// ServiceLeaseSLOEvidence is worker-reported operational evidence from actual
// bounded data-plane completions. It is not an independent availability or
// customer-path measurement and therefore remains explicit on the receipt.
type ServiceLeaseSLOEvidence struct {
	P95LatencyMillis            int64     `json:"p95_latency_milliseconds"`
	LatencyMeasurementCount     int       `json:"latency_measurement_count"`
	LatencyWindowSeconds        int64     `json:"latency_window_seconds"`
	LatencyMeasurementKind      string    `json:"latency_measurement_kind"`
	DataPlaneProbeReceiptSHA256 string    `json:"data_plane_probe_receipt_sha256"`
	MeasuredAt                  time.Time `json:"measured_at"`
}

type ServiceLease struct {
	ID                      uuid.UUID `json:"id"`
	BuyerID                 uuid.UUID `json:"buyer_id"`
	WorkerID                uuid.UUID `json:"worker_id"`
	SupplierID              uuid.UUID `json:"supplier_id"`
	RuntimeProfileID        string    `json:"runtime_profile_id"`
	RuntimeProfileSHA256    string    `json:"runtime_profile_sha256"`
	Region                  string    `json:"region"`
	MinimumReplicas         int       `json:"minimum_replicas"`
	MaximumReplicas         int       `json:"maximum_replicas"`
	MaximumP95LatencyMillis int64     `json:"maximum_p95_latency_milliseconds"`
	TermSeconds             int64     `json:"term_seconds"`
	State                   string    `json:"state"`
	ActiveReplicas          int       `json:"active_replicas"`
	UpgradeGeneration       string    `json:"upgrade_generation,omitempty"`
	// ReservedBuyerMicros is the exact prepaid maximum held for this lease.
	// It is a ledger-scale amount, never a USD display projection.
	ReservedBuyerMicros int64 `json:"reserved_buyer_micros"`
	// PricingAcceptanceID names the append-only authority used by current
	// admissions. A nil value is an explicit historical state: the lease
	// predates acceptance rows and replays its migration-frozen inline pair.
	PricingAcceptanceID    *uuid.UUID      `json:"pricing_acceptance_id,omitempty"`
	PricingAuthoritySource string          `json:"pricing_authority_source"`
	Pricing                PricingDecision `json:"pricing_decision"`
	PricingDecisionSHA256  string          `json:"pricing_decision_sha256"`
	StartedAt              time.Time       `json:"started_at"`
	ExpiresAt              time.Time       `json:"expires_at"`
	LastMeteredAt          time.Time       `json:"last_metered_at"`
	LastWorkerHeartbeatAt  time.Time       `json:"last_worker_heartbeat_at"`
	CumulativeReplicaNanos int64           `json:"cumulative_replica_nanoseconds"`
	BuyerChargeNanos       int64           `json:"buyer_charge_nanos"`
	SupplierPayableNanos   int64           `json:"supplier_payable_nanos"`
	KnownVariableCostNanos int64           `json:"known_variable_cost_nanos"`
	KnownContributionNanos int64           `json:"known_contribution_nanos"`
	FinalizedAt            *time.Time      `json:"finalized_at,omitempty"`
}

type ServiceLeaseReceipt struct {
	Lease                     ServiceLease                      `json:"lease"`
	BuyerFundingState         string                            `json:"buyer_funding_state"`
	SupplierSettlementState   string                            `json:"supplier_settlement_state"`
	TrueNetContributionStatus string                            `json:"true_net_contribution_status"`
	DataPlaneAuthorityStatus  string                            `json:"data_plane_authority_status"`
	ResidencyAuthorityStatus  string                            `json:"residency_authority_status"`
	EgressAuthorityStatus     string                            `json:"egress_authority_status"`
	ReserveRefundStatus       string                            `json:"reserve_refund_status"`
	ReceiptBlockers           []string                          `json:"receipt_blockers"`
	MeteringSemantics         string                            `json:"metering_semantics"`
	MarketClearing            *serviceLeaseMarketClearingDetail `json:"market_clearing,omitempty"`
	Settlement                *ServiceLeaseSettlement           `json:"settlement,omitempty"`
	LatestSLOEvidence         *ServiceLeaseSLOEvidence          `json:"latest_slo_evidence,omitempty"`
	DataPlaneDiagnostics      *ServiceLeaseDataPlaneDiagnostics `json:"data_plane_diagnostics,omitempty"`
}

// ServiceLeaseDataPlaneDiagnostics is a prompt-free aggregate of application
// payload sizes observed by the reserved proxy. It is useful for future egress
// reconciliation, but is explicitly not a provider invoice, wire-byte count,
// or settlement authority.
type ServiceLeaseDataPlaneDiagnostics struct {
	SuccessfulRequests       int64  `json:"successful_requests"`
	RequestApplicationBytes  int64  `json:"request_application_bytes"`
	ResponseApplicationBytes int64  `json:"response_application_bytes"`
	AuthorityStatus          string `json:"authority_status"`
}

// serviceLeaseActivationDetail is the immutable admission-side economic
// receipt. The append-only pricing acceptance keeps the canonical full
// PricingDecision (with an immutable inline lease projection for compatibility),
// while this compact event makes the selected supplier floor, every allocated
// cost, and the fixed-point conservation identity auditable from the event
// stream alone. It is deliberately explicit that processor fees are not yet
// allocated: a gross platform spread must never be mistaken for true net
// contribution.
type serviceLeaseActivationDetail struct {
	ReservedCeilingNanos             int64                             `json:"reserved_ceiling_nanos"`
	ReservedBuyerMicros              int64                             `json:"reserved_buyer_micros"`
	Currency                         string                            `json:"currency"`
	PricingDecisionSHA256            string                            `json:"pricing_decision_sha256"`
	PricingAuthorityVersion          int                               `json:"pricing_authority_version"`
	PricingPolicyRevision            string                            `json:"pricing_policy_revision"`
	RoundingPolicy                   string                            `json:"rounding_policy"`
	SupplierFloorNanosPerReplicaHour int64                             `json:"supplier_floor_nanos_per_replica_hour"`
	ResidencyNanosPerReplicaHour     int64                             `json:"residency_nanos_per_replica_hour"`
	ControlNanosPerReplicaHour       int64                             `json:"control_nanos_per_replica_hour"`
	RiskReserveNanosPerReplicaHour   int64                             `json:"risk_reserve_nanos_per_replica_hour"`
	ContributionNanosPerReplicaHour  int64                             `json:"contribution_nanos_per_replica_hour"`
	BuyerChargeNanos                 int64                             `json:"buyer_charge_nanos"`
	SupplierEntitlementsNanos        int64                             `json:"supplier_entitlements_nanos"`
	KnownVariableCostsNanos          int64                             `json:"known_variable_costs_nanos"`
	MercGrossSpreadNanos             int64                             `json:"merc_gross_spread_nanos"`
	KnownCostContributionNanos       int64                             `json:"known_cost_contribution_nanos"`
	TrueNetContributionStatus        string                            `json:"true_net_contribution_status"`
	UnknownCostCategories            []string                          `json:"unknown_cost_categories,omitempty"`
	ResidencyLiabilityPolicy         string                            `json:"residency_liability_policy"`
	MarketClearing                   *serviceLeaseMarketClearingDetail `json:"market_clearing,omitempty"`
}

func serviceLeaseActivationEventDetail(pricing PricingDecision, digest string, reservedBuyerMicros int64, market *serviceLeaseMarketClearingDetail) ([]byte, error) {
	authority := pricing.ServiceLease
	fixed := pricing.FixedPoint
	if authority == nil || fixed == nil || digest == "" || fixed.Currency != pricing.Currency {
		return nil, errors.New("service lease activation event lacks complete pricing authority")
	}
	detail := serviceLeaseActivationDetail{
		ReservedCeilingNanos:             fixed.AcceptedCeilingNanos,
		ReservedBuyerMicros:              reservedBuyerMicros,
		Currency:                         fixed.Currency,
		PricingDecisionSHA256:            digest,
		PricingAuthorityVersion:          authority.Version,
		PricingPolicyRevision:            pricing.PolicyRevision,
		RoundingPolicy:                   authority.RoundingPolicy,
		SupplierFloorNanosPerReplicaHour: authority.SupplierNanosPerReplicaHour,
		ResidencyNanosPerReplicaHour:     authority.ResidencyNanosPerReplicaHour,
		ControlNanosPerReplicaHour:       authority.ControlPlaneNanosPerReplicaHour,
		RiskReserveNanosPerReplicaHour:   authority.RiskReserveNanosPerReplicaHour,
		ContributionNanosPerReplicaHour:  authority.ContributionNanosPerReplicaHour,
		BuyerChargeNanos:                 fixed.BuyerChargeNanos,
		SupplierEntitlementsNanos:        fixed.SupplierEntitlementsNanos,
		KnownVariableCostsNanos:          fixed.KnownVariableCostsNanos,
		MercGrossSpreadNanos:             fixed.MercGrossSpreadNanos,
		KnownCostContributionNanos:       fixed.KnownCostContributionNanos,
		TrueNetContributionStatus:        "UNKNOWN_ECONOMIC_FINALITY_BLOCKERS",
		UnknownCostCategories:            append([]string(nil), fixed.UnknownCostCategories...),
		ResidencyLiabilityPolicy:         "SELECTED_SUPPLIER_ALL_IN_WARM_CAPACITY_ENTITLEMENT_V2",
		MarketClearing:                   market,
	}
	if authority.Version == serviceLeasePricingAuthorityLegacyVersion {
		detail.ResidencyLiabilityPolicy = "LEGACY_PLATFORM_VARIABLE_COST_BENEFICIARY_UNBOUND"
	}
	return json.Marshal(detail)
}

// ServiceLeaseSettlement is the terminal ledger projection of the cumulative
// meter. Under current authority PlatformGrossMicros contains only the control
// allocation plus modeled contribution: residency is paid in the selected
// supplier's all-in credit, and no lease risk reserve is charged. Historical v1
// receipts retain their accepted arithmetic and label the unresolved liability.
//
// MoneyFinalityStatus / EconomicFinalityStatus make terminal cash vs true-net
// finality explicit. EconomicFinal is never true while receipt blockers remain.
type ServiceLeaseSettlement struct {
	Currency               string                       `json:"currency"`
	BuyerChargeMicros      int64                        `json:"buyer_charge_micros"`
	PrepaidDebitMicros     int64                        `json:"prepaid_debit_micros"`
	SupplierCreditMicros   int64                        `json:"supplier_credit_micros"`
	PlatformGrossMicros    int64                        `json:"platform_gross_micros"`
	SupplierPayoutStatus   string                       `json:"supplier_payout_status"`
	FundingAuthorityState  string                       `json:"funding_authority_state"`
	SupplierCredits        []ServiceLeaseSupplierCredit `json:"supplier_credits"`
	PricingDecisionSHA256  string                       `json:"pricing_decision_sha256,omitempty"`
	MoneyFinalityStatus    string                       `json:"money_finality_status,omitempty"`
	EconomicFinalityStatus string                       `json:"economic_finality_status,omitempty"`
	EconomicFinal          bool                         `json:"economic_final"`
	FinalityBlockers       []string                     `json:"finality_blockers,omitempty"`
}

// ServiceLeaseSupplierCredit preserves the per-supplier terminal projection.
// Its sum is SupplierCreditMicros; each amount is projected once from that
// supplier's complete metered duration, never from a rounded heartbeat delta.
type ServiceLeaseSupplierCredit struct {
	SupplierID   uuid.UUID `json:"supplier_id"`
	CreditMicros int64     `json:"credit_micros"`
	PayoutStatus string    `json:"payout_status"`
	PayableNanos int64     `json:"payable_nanos"`
}

func serviceLeaseLedgerRef(leaseID uuid.UUID, kind string) string {
	return "service-lease-ledger-" + leaseID.String() + ":" + kind
}

func serviceLeaseSupplierCreditLedgerRef(leaseID, supplierID uuid.UUID) string {
	return serviceLeaseLedgerRef(leaseID, KindSupplierCredit) + ":" + supplierID.String()
}

// serviceLeaseIDFromSupplierCreditRef is the inverse of the immutable
// supplier-credit reference. Service lease credits have no task_id, so payout
// funding must recover the exact terminal lease from this structured reference
// rather than inventing a job or trusting the lease's mutable current worker.
func serviceLeaseIDFromSupplierCreditRef(ref string) (uuid.UUID, bool) {
	const prefix = "service-lease-ledger-"
	if !strings.HasPrefix(ref, prefix) {
		return uuid.Nil, false
	}
	parts := strings.Split(strings.TrimPrefix(ref, prefix), ":")
	if len(parts) != 3 || parts[1] != KindSupplierCredit {
		return uuid.Nil, false
	}
	leaseID, err := uuid.Parse(parts[0])
	if err != nil || leaseID == uuid.Nil {
		return uuid.Nil, false
	}
	if supplierID, err := uuid.Parse(parts[2]); err != nil || supplierID == uuid.Nil {
		return uuid.Nil, false
	}
	return leaseID, true
}

// insertServiceLeaseLedgerEntryTx provides idempotence for a service ledger
// row without fabricating a task. The existing task/kind unique key cannot be
// used here because service leases are long-running capacity commitments, not
// batch tasks. Payout references are opaque immutable identifiers and are also
// how the receipt reassembles the terminal buyer/prepaid/platform and
// per-supplier records.
func insertServiceLeaseLedgerEntryTx(ctx context.Context, tx pgx.Tx, leaseID uuid.UUID, reference string, entry ledgerInsert) error {
	if leaseID == uuid.Nil {
		return errors.New("service ledger entry requires a lease")
	}
	if reference == "" {
		return errors.New("service ledger entry requires an immutable reference")
	}
	entry.PayoutRef = reference
	ct, err := insertLedgerEntryIfAbsentByRefTx(ctx, tx, entry)
	if err != nil || ct.RowsAffected() == 1 {
		return err
	}
	resolved, err := resolveLedgerInsert(entry)
	if err != nil {
		return err
	}
	var (
		amountMicros        int64
		buyerID, supplierID *uuid.UUID
		currency            string
	)
	if err := tx.QueryRow(ctx, `SELECT (amount_usd*1000000)::bigint,buyer_id,supplier_id,currency
		FROM ledger_entries WHERE kind=$1 AND payout_ref=$2`, resolved.Kind, resolved.PayoutRef).
		Scan(&amountMicros, &buyerID, &supplierID, &currency); err != nil {
		return err
	}
	if amountMicros != resolved.AmountMicros || !sameOptionalUUID(buyerID, resolved.BuyerID) ||
		!sameOptionalUUID(supplierID, resolved.SupplierID) || currency != resolved.Currency {
		return fmt.Errorf("conflicting terminal service ledger entry %s for lease %s", resolved.Kind, leaseID)
	}
	return nil
}

// settleFinalServiceLeaseTx records terminal service money exactly once. The
// cumulative PricingDecision is projected once at ledger precision, never once
// per heartbeat, so micro-rounding cannot turn a legal ceiling into a hidden
// subsidy or an overcharge. Supplier credit is deliberately held: collected
// prepaid cash is real buyer funding, but the payout rail has not yet bound a
// particular top-up collection to this service liability.
func settleFinalServiceLeaseTx(ctx context.Context, tx pgx.Tx, lease *ServiceLease) error {
	if lease == nil || lease.ID == uuid.Nil {
		return errors.New("service terminal settlement requires an accepted lease")
	}
	if lease.ReservedBuyerMicros <= 0 {
		if lease.PricingAcceptanceID != nil {
			return errors.New("accepted service lease has no positive frozen prepaid reservation")
		}
		// Explicit legacy rows predate collected-prepaid acceptance. They remain
		// readable/replayable but must not acquire invented cash facts.
		return nil
	}
	if err := validateServiceLeaseAcceptedPricingBinding(*lease); err != nil {
		return err
	}
	// Re-seal the pricing digest at terminal settle so the ledger citation
	// matches the accept-time authority (accept already sealed it on the row).
	gotSHA, err := pricingDecisionDigest(lease.Pricing)
	if err != nil {
		return fmt.Errorf("service lease terminal pricing digest: %w", err)
	}
	if lease.PricingDecisionSHA256 == "" || gotSHA != lease.PricingDecisionSHA256 {
		return fmt.Errorf("service lease terminal pricing digest mismatch for lease %s", lease.ID)
	}
	liabilityAuth := liabilityAuthority{
		PricingDecisionSHA256: lease.PricingDecisionSHA256,
		LaneSettlementID:      lease.ID.String(),
	}
	if err := liabilityAuth.validate(); err != nil {
		return err
	}
	currency, err := ParseCurrency(lease.Pricing.Currency)
	if err != nil {
		return fmt.Errorf("service settlement currency: %w", err)
	}
	buyerMicros, err := LedgerMicrosFromNanos(MoneyNanos{Currency: currency, Nanos: lease.BuyerChargeNanos})
	if err != nil {
		return err
	}
	payables, err := serviceLeaseSupplierPayables(ctx, tx, lease.ID)
	if err != nil {
		return err
	}
	var payableNanos, supplierMicros int64
	for _, payable := range payables {
		if payable.PayableNanos > int64(^uint64(0)>>1)-payableNanos {
			return errors.New("service supplier payable attribution overflow")
		}
		payableNanos += payable.PayableNanos
		if payable.PayableNanos == 0 {
			continue
		}
		creditMicros, err := LedgerMicrosFromNanos(MoneyNanos{Currency: currency, Nanos: payable.PayableNanos})
		if err != nil {
			return err
		}
		if creditMicros <= 0 || creditMicros > int64(^uint64(0)>>1)-supplierMicros {
			return errors.New("service supplier ledger projection is invalid")
		}
		supplierMicros += creditMicros
		supplier := payable.SupplierID
		credit := ledgerInsert{
			Kind: KindSupplierCredit, SupplierID: &supplier, AmountMicros: creditMicros,
			Currency: currency.Code(), PayoutStatus: PayoutHeld,
			ReleaseAt: ptrTime(payoutReleaseAt(time.Now().UTC(), 0)),
		}
		if err := applyLiabilityAuthority(&credit, liabilityAuth); err != nil {
			return err
		}
		if err := insertServiceLeaseLedgerEntryTx(ctx, tx, lease.ID,
			serviceLeaseSupplierCreditLedgerRef(lease.ID, supplier), credit); err != nil {
			return err
		}
	}
	if payableNanos != lease.SupplierPayableNanos || buyerMicros <= 0 || supplierMicros <= 0 ||
		supplierMicros > buyerMicros || buyerMicros > lease.ReservedBuyerMicros {
		return fmt.Errorf("service terminal ledger projection violates frozen prepaid or supplier bounds: buyer=%d supplier=%d reserved=%d attributed_nanos=%d aggregate_nanos=%d",
			buyerMicros, supplierMicros, lease.ReservedBuyerMicros, payableNanos, lease.SupplierPayableNanos)
	}
	platformMicros := buyerMicros - supplierMicros
	if platformMicros < 0 {
		return errors.New("service terminal ledger projection has negative platform gross")
	}
	buyer := lease.BuyerID
	buyerCharge := ledgerInsert{
		Kind: KindBuyerCharge, BuyerID: &buyer, AmountMicros: -buyerMicros,
		Currency: currency.Code(), PayoutStatus: PayoutReleased,
	}
	if err := applyLiabilityAuthority(&buyerCharge, liabilityAuth); err != nil {
		return err
	}
	if err := insertServiceLeaseLedgerEntryTx(ctx, tx, lease.ID, serviceLeaseLedgerRef(lease.ID, KindBuyerCharge), buyerCharge); err != nil {
		return err
	}
	if err := debitPrepaidForServiceLeaseTx(ctx, tx, buyer, lease.ID, buyerMicros); err != nil {
		return err
	}
	platformTake := ledgerInsert{
		Kind: KindPlatformTake, AmountMicros: platformMicros,
		Currency: currency.Code(), PayoutStatus: PayoutReleased,
	}
	if err := applyLiabilityAuthority(&platformTake, liabilityAuth); err != nil {
		return err
	}
	return insertServiceLeaseLedgerEntryTx(ctx, tx, lease.ID, serviceLeaseLedgerRef(lease.ID, KindPlatformTake), platformTake)
}

func ptrTime(value time.Time) *time.Time { return &value }

type serviceLeaseSupplierPayable struct {
	SupplierID   uuid.UUID
	PayableNanos int64
}

type serviceLeaseMeterReader interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// serviceLeaseSupplierPayables reconstructs the payout split exclusively from
// append-only meter deltas. A lease may change workers during failover; its
// mutable current supplier must never be used to attribute earlier runtime.
func serviceLeaseSupplierPayables(ctx context.Context, db serviceLeaseMeterReader, leaseID uuid.UUID) ([]serviceLeaseSupplierPayable, error) {
	rows, err := db.Query(ctx, `SELECT supplier_id,COALESCE(sum(supplier_payable_delta_nanos),0)::bigint
		FROM service_lease_supplier_meterings WHERE lease_id=$1
		GROUP BY supplier_id ORDER BY supplier_id`, leaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	payables := make([]serviceLeaseSupplierPayable, 0)
	for rows.Next() {
		var payable serviceLeaseSupplierPayable
		if err := rows.Scan(&payable.SupplierID, &payable.PayableNanos); err != nil {
			return nil, err
		}
		if payable.SupplierID == uuid.Nil || payable.PayableNanos < 0 {
			return nil, errors.New("service supplier meter contains invalid payable attribution")
		}
		payables = append(payables, payable)
	}
	return payables, rows.Err()
}

func validateServiceLeaseOffer(reg ServiceLeaseOfferRegistration) (VLLMRuntimeProfile, error) {
	if _, err := validateCurrentServiceLeaseCurrency(reg.Currency); err != nil {
		return VLLMRuntimeProfile{}, err
	}
	profile, ok := vllmProfileByID(strings.TrimSpace(reg.RuntimeProfileID))
	if !ok || profile.ProfileSHA256 != reg.RuntimeProfileSHA256 {
		return VLLMRuntimeProfile{}, errors.New("service lease offer runtime profile does not match authority")
	}
	if !serviceLeaseRegionPattern.MatchString(reg.Region) || reg.MaximumWarmReplicas < 1 ||
		reg.AvailableWarmReplicas < 0 || reg.AvailableWarmReplicas > reg.MaximumWarmReplicas ||
		reg.SupplierNanosPerReplicaHour <= 0 || reg.ResidencyNanosPerReplicaHour <= 0 ||
		reg.P95LatencyMillis <= 0 || reg.LatencyMeasurementCount < 5 || reg.LatencyWindowSeconds < 1 ||
		reg.LatencyWindowSeconds > 300 || reg.LatencyMeasurementKind != "DATA_PLANE_COMPLETIONS_V1" {
		return VLLMRuntimeProfile{}, errors.New("service lease offer has invalid capacity, region, or exact floor")
	}
	switch reg.Status {
	case "READY", "DRAINING", "FAILED":
	default:
		return VLLMRuntimeProfile{}, errors.New("service lease offer has invalid status")
	}
	return profile, nil
}

func (s *Store) UpsertServiceLeaseOffer(ctx context.Context, auth WorkerAuth, reg ServiceLeaseOfferRegistration) error {
	if auth.WorkerID == uuid.Nil || auth.SupplierID == uuid.Nil {
		return errors.New("service lease offer requires worker and supplier identity")
	}
	profile, err := validateServiceLeaseOffer(reg)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Materialise then lock the offer row before reading reservations. Creation,
	// failover, expiry, and a worker refresh all serialize on this row: otherwise
	// a normal periodic agent refresh could overwrite the decrement made when a
	// buyer reserved warm capacity and overbook the host.
	if _, err := tx.Exec(ctx, `
		INSERT INTO service_lease_worker_offers
			 (worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,region,currency,
			  maximum_warm_replicas,available_warm_replicas,supplier_nanos_per_replica_hour,
			  residency_nanos_per_replica_hour,supports_rolling_upgrade,p95_latency_milliseconds,
			  latency_measurement_count,latency_window_seconds,latency_measurement_kind,status,last_seen_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,now(),now())
		ON CONFLICT (worker_id,runtime_profile_id,region) DO NOTHING`,
		auth.WorkerID, auth.SupplierID, reg.RuntimeProfileID, reg.RuntimeProfileSHA256, reg.Region, reg.Currency,
		reg.MaximumWarmReplicas, reg.AvailableWarmReplicas, reg.SupplierNanosPerReplicaHour,
		reg.ResidencyNanosPerReplicaHour, reg.SupportsRollingUpgrade, reg.P95LatencyMillis,
		reg.LatencyMeasurementCount, reg.LatencyWindowSeconds, reg.LatencyMeasurementKind, reg.Status); err != nil {
		return err
	}
	var locked int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM service_lease_worker_offers
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3 FOR UPDATE`,
		auth.WorkerID, reg.RuntimeProfileID, reg.Region).Scan(&locked); err != nil {
		return err
	}
	reserved, err := activeServiceLeaseReservationTx(ctx, tx, auth.WorkerID, reg.RuntimeProfileID, reg.Region)
	if err != nil {
		return err
	}
	if reserved > reg.MaximumWarmReplicas {
		return errServiceLeaseOfferBelowReservations
	}
	available := reg.AvailableWarmReplicas
	if free := reg.MaximumWarmReplicas - reserved; available > free {
		available = free
	}
	if reg.Status != "READY" {
		available = 0
	}
	// Join declared warm capacity to measured worker_model_state. An offer may
	// not advertise warm replicas the worker's own measured residency does not
	// support. Unmeasurable (no fresh measured row for the profile model) fails
	// closed to zero — never fall back to the operator-declared TOML value.
	measured, err := measuredServiceLeaseWarmCapacityTx(ctx, tx, auth.WorkerID, profile.ModelAlias, reg.MaximumWarmReplicas)
	if err != nil {
		return err
	}
	if available > measured {
		available = measured
	}
	if _, err := tx.Exec(ctx, `UPDATE service_lease_worker_offers SET
		supplier_id=$4,currency=$5,maximum_warm_replicas=$6,available_warm_replicas=$7,
		supplier_nanos_per_replica_hour=$8,residency_nanos_per_replica_hour=$9,
		supports_rolling_upgrade=$10,p95_latency_milliseconds=$11,
		latency_measurement_count=$12,latency_window_seconds=$13,
		latency_measurement_kind=$14,status=$15,last_seen_at=now(),updated_at=now()
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`,
		auth.WorkerID, reg.RuntimeProfileID, reg.Region, auth.SupplierID, reg.Currency,
		reg.MaximumWarmReplicas, available, reg.SupplierNanosPerReplicaHour,
		reg.ResidencyNanosPerReplicaHour, reg.SupportsRollingUpgrade, reg.P95LatencyMillis,
		reg.LatencyMeasurementCount, reg.LatencyWindowSeconds, reg.LatencyMeasurementKind, reg.Status); err != nil {
		return err
	}
	if err := recordServiceLeaseOfferSampleTx(ctx, tx, auth.WorkerID, reg.RuntimeProfileID, reg.Region); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// recordServiceLeaseOfferSampleTx snapshots the mutable offer after a real
// control-plane change. It is observational only, but sharing this helper with
// reservation/finalization paths prevents a utilization report from seeing
// offer refreshes while missing the capacity mutations that matter most.
func recordServiceLeaseOfferSampleTx(ctx context.Context, tx pgx.Tx, workerID uuid.UUID, runtimeProfileID, region string) error {
	var (
		supplierID, workerHW, profileSHA, status string
		maximum, available                       int
		supplierRate, residencyRate              int64
		currency                                 *string
	)
	if err := tx.QueryRow(ctx, `SELECT o.supplier_id,COALESCE(NULLIF(btrim(w.hw_class),''),'UNDECLARED'),
		o.runtime_profile_sha256,o.status,o.maximum_warm_replicas,o.available_warm_replicas,
		o.supplier_nanos_per_replica_hour,o.residency_nanos_per_replica_hour,o.currency
		FROM service_lease_worker_offers o JOIN workers w ON w.id=o.worker_id
		WHERE o.worker_id=$1 AND o.runtime_profile_id=$2 AND o.region=$3`, workerID, runtimeProfileID, region).Scan(
		&supplierID, &workerHW, &profileSHA, &status, &maximum, &available, &supplierRate, &residencyRate, &currency); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO service_lease_offer_samples
		(worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,region,worker_declared_hw_class,
		 status,maximum_warm_replicas,available_warm_replicas,supplier_nanos_per_replica_hour,
		 residency_nanos_per_replica_hour,currency)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, workerID, supplierID, runtimeProfileID,
		profileSHA, region, workerHW, status, maximum, available, supplierRate, residencyRate, currency)
	return err
}

// measuredServiceLeaseWarmCapacityTx returns how many warm replicas the
// worker's measured residency currently supports for this profile's model.
//
// worker_model_state is one row per model, not per replica: a fresh measured
// row (rss_delta_bytes and load_ms both set, last_seen_warm within the warmth
// TTL) means residency is observed and the declared maximum may be advertised.
// No measured row means the offer is unmeasurable and must advertise zero.
// Declared-only loaded_models rows (NULL measurements) do not count.
func measuredServiceLeaseWarmCapacityTx(ctx context.Context, tx pgx.Tx, workerID uuid.UUID, modelAlias string, maximumWarmReplicas int) (int, error) {
	if workerID == uuid.Nil || strings.TrimSpace(modelAlias) == "" || maximumWarmReplicas < 1 {
		return 0, nil
	}
	var measured bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM worker_model_state
			 WHERE worker_id = $1
			   AND model_id = $2
			   AND last_seen_warm > now() - interval '60 seconds'
			   AND rss_delta_bytes IS NOT NULL
			   AND load_ms IS NOT NULL
		)`, workerID, modelAlias).Scan(&measured)
	if err != nil {
		return 0, err
	}
	if !measured {
		return 0, nil
	}
	return maximumWarmReplicas, nil
}

// activeServiceLeaseReservationTx returns capacity still promised on this
// exact worker/profile/region. A worker that reported failure continues to
// hold its allocation until a successful failover rewrites the lease, so it
// cannot immediately advertise the same replicas to another buyer.
func activeServiceLeaseReservationTx(ctx context.Context, tx pgx.Tx, workerID uuid.UUID, runtimeProfileID, region string) (int, error) {
	rows, err := tx.Query(ctx, `SELECT maximum_replicas FROM service_leases
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3
		  AND state IN ('ACTIVE','UPGRADING','FAILOVER_REQUIRED') AND expires_at>now()
		FOR UPDATE`, workerID, runtimeProfileID, region)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	reserved := 0
	for rows.Next() {
		var replicas int
		if err := rows.Scan(&replicas); err != nil {
			return 0, err
		}
		if replicas < 1 || reserved > int(^uint(0)>>1)-replicas {
			return 0, errors.New("service lease reserved replica count is invalid")
		}
		reserved += replicas
	}
	return reserved, rows.Err()
}

func serviceLeasePricingInputs(profile VLLMRuntimeProfile, currency Currency, request ServiceLeaseRequest, supplierRate, residencyRate int64) ServiceLeasePricingInputs {
	return ServiceLeasePricingInputs{
		Profile: profile, Currency: currency, Region: request.Region,
		MinimumReplicas: request.MinimumReplicas, MaximumReplicas: request.MaximumReplicas,
		TermSeconds: request.TermSeconds, MaximumP95LatencyMilliseconds: request.MaximumP95LatencyMilliseconds,
		SupplierNanosPerReplicaHour: supplierRate, ResidencyNanosPerReplicaHour: residencyRate,
		ControlPlaneNanosPerReplicaHour: serviceLeaseControlUSDNanosHour,
		RiskReserveNanosPerReplicaHour:  0,
		ContributionNanosPerReplicaHour: serviceLeaseContributionUSDHour,
		BuyerDeclaredCeilingNanos:       request.BuyerDeclaredCeilingNanos,
	}
}

func (s *Store) CreateServiceLease(ctx context.Context, buyerID uuid.UUID, request ServiceLeaseRequest) (ServiceLease, error) {
	if buyerID == uuid.Nil || !serviceLeaseRegionPattern.MatchString(request.Region) || request.BuyerDeclaredCeilingNanos <= 0 {
		return ServiceLease{}, errors.New("service lease request has invalid buyer, region, or ceiling")
	}
	currency, err := validateCurrentServiceLeaseCurrency(request.Currency)
	if err != nil {
		return ServiceLease{}, err
	}
	profile, ok := vllmProfileByID(request.RuntimeProfileID)
	if !ok {
		return ServiceLease{}, errors.New("unknown service lease runtime profile")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ServiceLease{}, err
	}
	defer tx.Rollback(ctx)

	// This is a real buyer order crossing a live supplier offer book. Lock every
	// compatible candidate in deterministic ask order until the lease commits so
	// the selected rank and candidate depth cannot describe a market that changed
	// underneath the reservation. The canonical PricingDecision still decides
	// whether an ask clears the buyer ceiling and preserves positive contribution.
	//
	// Region remains a hard filter (WHERE region=$3). Ranking is total measured
	// cost — supplier ask plus residency nanos per replica hour — so residency
	// is honoured in the order book, not only as admission. A cheap supplier
	// with expensive residency must not clear above a slightly higher supplier
	// ask whose residency is lower when the sum says so.
	rows, err := tx.Query(ctx, `
		SELECT worker_id,supplier_id,supplier_nanos_per_replica_hour,
		       residency_nanos_per_replica_hour,available_warm_replicas
		  FROM service_lease_worker_offers
		 WHERE runtime_profile_id=$1 AND runtime_profile_sha256=$2 AND region=$3 AND currency=$6 AND status='READY'
		   AND p95_latency_milliseconds>0 AND latency_measurement_count>=5
		   AND latency_window_seconds BETWEEN 1 AND 300 AND latency_measurement_kind='DATA_PLANE_COMPLETIONS_V1'
		   AND p95_latency_milliseconds <= $5 AND last_seen_at > now()-interval '45 seconds' AND available_warm_replicas >= $4
		 ORDER BY (supplier_nanos_per_replica_hour + residency_nanos_per_replica_hour) ASC,
		          supplier_nanos_per_replica_hour ASC,worker_id ASC
		 FOR UPDATE`, profile.RuntimeProfileID, profile.ProfileSHA256, request.Region, request.MaximumReplicas, request.MaximumP95LatencyMilliseconds, currency.Code())
	if err != nil {
		return ServiceLease{}, err
	}
	defer rows.Close()
	candidates := make([]serviceLeaseMarketCandidate, 0)
	for rows.Next() {
		var candidate serviceLeaseMarketCandidate
		if err := rows.Scan(&candidate.WorkerID, &candidate.SupplierID,
			&candidate.SupplierNanosPerReplicaHour, &candidate.ResidencyNanosPerReplicaHour,
			&candidate.AvailableWarmReplicas); err != nil {
			return ServiceLease{}, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return ServiceLease{}, err
	}
	if len(candidates) == 0 {
		return ServiceLease{}, errRealtimeNoSupply
	}
	var (
		selectedCandidate serviceLeaseMarketCandidate
		selectedPricing   PricingDecision
		selectedRank      int
	)
	for i, candidate := range candidates {
		candidatePricing, pricingErr := newServiceLeasePricingDecision(
			serviceLeasePricingInputs(profile, currency, request,
				candidate.SupplierNanosPerReplicaHour, candidate.ResidencyNanosPerReplicaHour))
		if pricingErr != nil {
			// An ask that cannot clear the frozen buyer ceiling or the positive
			// contribution invariant is not silently admitted. Continue only to
			// another measured ask; if none clears, the order is refused.
			continue
		}
		selectedCandidate, selectedPricing, selectedRank = candidate, candidatePricing, i+1
		break
	}
	if selectedRank == 0 {
		return ServiceLease{}, errRealtimeNoSupply
	}
	workerID, supplierID := selectedCandidate.WorkerID, selectedCandidate.SupplierID
	supplierRate, residencyRate := selectedCandidate.SupplierNanosPerReplicaHour, selectedCandidate.ResidencyNanosPerReplicaHour
	pricing := selectedPricing
	reservedMicros, err := LedgerMicrosFromNanos(MoneyNanos{Currency: currency, Nanos: pricing.FixedPoint.AcceptedCeilingNanos})
	if err != nil || reservedMicros <= 0 {
		if err == nil {
			err = errors.New("service lease accepted ceiling has no positive prepaid projection")
		}
		return ServiceLease{}, err
	}
	// Services are not allowed to borrow from free_credit_usd, deferred-card
	// exposure, or a future charge. A warm replica burns immediately, so its
	// full frozen ceiling has to be reserved against collected prepaid cash
	// before the supplier capacity is removed from the market.
	if err := reservePrepaidForServiceLeaseTx(ctx, tx, buyerID, reservedMicros); err != nil {
		if errors.Is(err, errInsufficientPrepaid) {
			return ServiceLease{}, errRealtimeInsufficientFunds
		}
		return ServiceLease{}, err
	}
	pricingJSON, err := json.Marshal(pricing)
	if err != nil {
		return ServiceLease{}, err
	}
	pricingSHA, err := pricingDecisionDigest(pricing)
	if err != nil {
		return ServiceLease{}, err
	}
	// Service lease freezes worker + pricing, not multi-host topology. Record
	// an explicit NOT_APPLICABLE TopologyDecision so accept never binds neither.
	topologyDecision, err := buildServiceLeaseTopologyDecision()
	if err != nil {
		return ServiceLease{}, err
	}
	topologyJSON, err := json.Marshal(topologyDecision)
	if err != nil {
		return ServiceLease{}, err
	}
	topologySHA, err := topologyDecisionDigest(topologyDecision)
	if err != nil {
		return ServiceLease{}, err
	}
	now := time.Now().UTC()
	leaseID := uuid.New()
	lease := ServiceLease{ID: leaseID, BuyerID: buyerID, WorkerID: workerID, SupplierID: supplierID,
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		Region: request.Region, MinimumReplicas: request.MinimumReplicas, MaximumReplicas: request.MaximumReplicas,
		MaximumP95LatencyMillis: request.MaximumP95LatencyMilliseconds, TermSeconds: request.TermSeconds,
		State: "ACTIVE", ActiveReplicas: request.MinimumReplicas,
		PricingAcceptanceID: &leaseID, PricingAuthoritySource: serviceLeasePricingSourceAcceptance,
		Pricing: pricing, PricingDecisionSHA256: pricingSHA,
		ReservedBuyerMicros: reservedMicros,
		StartedAt:           now, ExpiresAt: now.Add(time.Duration(request.TermSeconds) * time.Second),
		LastMeteredAt: now, LastWorkerHeartbeatAt: now}
	if err := insertServiceLeasePricingAcceptanceTx(ctx, tx, lease.ID, pricingJSON, pricingSHA); err != nil {
		return ServiceLease{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO service_leases
		 (id,buyer_id,worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,region,
		  minimum_replicas,maximum_replicas,maximum_p95_latency_milliseconds,term_seconds,state,
		  active_replicas,reserved_buyer_micros,pricing_acceptance_id,pricing_decision,pricing_decision_sha256,
		  topology_decision,topology_decision_sha256,
		  started_at,expires_at,last_metered_at,last_worker_heartbeat_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'ACTIVE',$8,$12,$1,$13,$14,$15,$16,$17,$18,$17,$17)`,
		lease.ID, lease.BuyerID, lease.WorkerID, lease.SupplierID, lease.RuntimeProfileID,
		lease.RuntimeProfileSHA256, lease.Region, lease.MinimumReplicas, lease.MaximumReplicas,
		lease.MaximumP95LatencyMillis, lease.TermSeconds, lease.ReservedBuyerMicros,
		pricingJSON, pricingSHA, topologyJSON, topologySHA, now, lease.ExpiresAt)
	if err != nil {
		return ServiceLease{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE service_lease_worker_offers
		SET available_warm_replicas=available_warm_replicas-$4,updated_at=now()
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3 AND available_warm_replicas >= $4`,
		workerID, profile.RuntimeProfileID, request.Region, request.MaximumReplicas); err != nil {
		return ServiceLease{}, err
	}
	if err := recordServiceLeaseOfferSampleTx(ctx, tx, workerID, profile.RuntimeProfileID, request.Region); err != nil {
		return ServiceLease{}, err
	}
	market := &serviceLeaseMarketClearingDetail{
		Version:                    serviceLeaseMarketClearingVersion,
		CandidateCount:             len(candidates),
		SelectedRank:               selectedRank,
		SelectedWorkerID:           workerID,
		SelectedSupplierID:         supplierID,
		SelectedSupplierRateNanos:  supplierRate,
		SelectedResidencyRateNanos: residencyRate,
		BuyerCeilingNanos:          request.BuyerDeclaredCeilingNanos,
		AcceptedCeilingNanos:       pricing.FixedPoint.AcceptedCeilingNanos,
		PricingDecisionSHA256:      pricingSHA,
		PositiveContributionNanos:  pricing.FixedPoint.KnownCostContributionNanos,
		OrderBookPolicy:            "lowest_total_supplier_plus_residency_ask_v1",
		SelectionReason:            "lowest total measured cost (supplier + residency nanos per replica hour) cleared the buyer ceiling with positive fixed-point contribution",
	}
	activationDetail, err := serviceLeaseActivationEventDetail(pricing, pricingSHA, lease.ReservedBuyerMicros, market)
	if err != nil {
		return ServiceLease{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO service_lease_events (lease_id,kind,detail)
		VALUES ($1,'ACTIVATED',$2::jsonb)`, lease.ID, activationDetail); err != nil {
		return ServiceLease{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ServiceLease{}, err
	}
	return lease, nil
}

func decodeServiceLeasePricing(raw []byte, digest string) (PricingDecision, error) {
	var pricing PricingDecision
	if err := json.Unmarshal(raw, &pricing); err != nil {
		return PricingDecision{}, err
	}
	actual, err := pricingDecisionDigest(pricing)
	if err != nil || actual != digest || pricing.ExecutionMode != pricingExecutionServiceLease || pricing.ServiceLease == nil {
		return PricingDecision{}, errors.New("service lease pricing authority digest is invalid")
	}
	if err := validatePricingCostShape(pricing); err != nil {
		return PricingDecision{}, err
	}
	return pricing, nil
}

const (
	serviceLeasePricingSourceAcceptance = "APPEND_ONLY_ACCEPTANCE_V1"
	serviceLeasePricingSourceLegacy     = "LEGACY_INLINE_FROZEN_AT_MIGRATION"
)

// insertServiceLeasePricingAcceptanceTx is the only production writer for an
// accepted service price. The database makes the resulting row append-only;
// this store boundary additionally proves that the supplied digest is the
// canonical digest of a complete service-lease PricingDecision before either
// the acceptance or its lease projection can become visible.
func insertServiceLeasePricingAcceptanceTx(ctx context.Context, tx pgx.Tx, leaseID uuid.UUID, raw []byte, digest string) error {
	if leaseID == uuid.Nil {
		return errors.New("service lease pricing acceptance lacks lease identity")
	}
	if _, err := decodeServiceLeasePricing(raw, digest); err != nil {
		return fmt.Errorf("validate service lease pricing acceptance: %w", err)
	}
	_, err := tx.Exec(ctx, `INSERT INTO service_lease_pricing_acceptances
		(id,pricing_decision,pricing_decision_sha256)
		VALUES ($1,$2::jsonb,$3)`, leaseID, raw, digest)
	return err
}

func validateServiceLeaseAcceptedPricingBinding(lease ServiceLease) error {
	authority := lease.Pricing.ServiceLease
	if authority == nil || lease.Pricing.FixedPoint == nil {
		return errors.New("service lease lacks accepted pricing authority")
	}
	if authority.RuntimeProfileID != lease.RuntimeProfileID ||
		authority.RuntimeProfileSHA256 != lease.RuntimeProfileSHA256 ||
		authority.Region != lease.Region ||
		authority.MinimumReplicas != lease.MinimumReplicas ||
		authority.MaximumReplicas != lease.MaximumReplicas ||
		authority.TermSeconds != lease.TermSeconds ||
		authority.MaximumP95LatencyMilliseconds != lease.MaximumP95LatencyMillis {
		return errors.New("service lease row differs from its accepted pricing authority")
	}
	if lease.PricingAcceptanceID != nil && *lease.PricingAcceptanceID != lease.ID {
		return errors.New("service lease pricing acceptance reference differs from lease identity")
	}
	if lease.PricingAcceptanceID != nil && lease.ReservedBuyerMicros <= 0 {
		return errors.New("accepted service lease must retain a positive prepaid reservation")
	}
	if lease.ReservedBuyerMicros > 0 {
		currency, err := ParseCurrency(lease.Pricing.Currency)
		if err != nil {
			return err
		}
		reserved, err := LedgerMicrosFromNanos(MoneyNanos{
			Currency: currency,
			Nanos:    lease.Pricing.FixedPoint.AcceptedCeilingNanos,
		})
		if err != nil || reserved != lease.ReservedBuyerMicros {
			return errors.New("service lease prepaid reservation differs from accepted pricing ceiling")
		}
	}
	return nil
}

func scanServiceLease(row pgx.Row) (ServiceLease, error) {
	var lease ServiceLease
	var raw []byte
	err := row.Scan(&lease.ID, &lease.BuyerID, &lease.WorkerID, &lease.SupplierID,
		&lease.RuntimeProfileID, &lease.RuntimeProfileSHA256, &lease.Region, &lease.MinimumReplicas,
		&lease.MaximumReplicas, &lease.MaximumP95LatencyMillis, &lease.TermSeconds, &lease.State,
		&lease.ActiveReplicas, &lease.UpgradeGeneration, &lease.ReservedBuyerMicros, &lease.PricingAcceptanceID,
		&raw, &lease.PricingDecisionSHA256,
		&lease.StartedAt, &lease.ExpiresAt, &lease.LastMeteredAt, &lease.LastWorkerHeartbeatAt,
		&lease.CumulativeReplicaNanos, &lease.BuyerChargeNanos, &lease.SupplierPayableNanos,
		&lease.KnownVariableCostNanos, &lease.KnownContributionNanos, &lease.FinalizedAt)
	if err != nil {
		return ServiceLease{}, err
	}
	lease.Pricing, err = decodeServiceLeasePricing(raw, lease.PricingDecisionSHA256)
	if err != nil {
		return ServiceLease{}, err
	}
	if lease.PricingAcceptanceID == nil {
		lease.PricingAuthoritySource = serviceLeasePricingSourceLegacy
	} else {
		lease.PricingAuthoritySource = serviceLeasePricingSourceAcceptance
	}
	if err := validateServiceLeaseAcceptedPricingBinding(lease); err != nil {
		return ServiceLease{}, err
	}
	return lease, nil
}

const serviceLeaseColumns = `id,buyer_id,worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,
 region,minimum_replicas,maximum_replicas,maximum_p95_latency_milliseconds,term_seconds,state,
 active_replicas,upgrade_generation,reserved_buyer_micros,pricing_acceptance_id,
 CASE WHEN pricing_acceptance_id IS NULL THEN pricing_decision ELSE
   (SELECT accepted.pricing_decision FROM service_lease_pricing_acceptances accepted
     WHERE accepted.id=pricing_acceptance_id) END,
 CASE WHEN pricing_acceptance_id IS NULL THEN pricing_decision_sha256 ELSE
   (SELECT accepted.pricing_decision_sha256 FROM service_lease_pricing_acceptances accepted
     WHERE accepted.id=pricing_acceptance_id) END,
 started_at,expires_at,
 last_metered_at,last_worker_heartbeat_at,cumulative_replica_nanoseconds,buyer_charge_nanos,
 supplier_payable_nanos,known_variable_cost_nanos,known_contribution_nanos,finalized_at`

func serviceLeaseKnownVariableNanos(authority ServiceLeasePricingAuthority, money ServiceLeaseMoney) (int64, error) {
	if authority.Version == serviceLeasePricingAuthorityVersion {
		// Residency is inside SupplierPayable for the current authority. Counting
		// it here as well would both double-count the buyer charge and let the
		// platform present a supplier liability as its own variable cost.
		return money.ControlPlaneCost.Nanos, nil
	}
	variable := money.ResidencyCost.Nanos + money.ControlPlaneCost.Nanos + money.RiskReserve.Nanos
	if variable < money.ResidencyCost.Nanos || variable < money.ControlPlaneCost.Nanos {
		return 0, errors.New("historical service lease variable cost overflow")
	}
	return variable, nil
}

func meterServiceLeaseTx(ctx context.Context, tx pgx.Tx, lease *ServiceLease, at time.Time) error {
	if at.Before(lease.LastMeteredAt) {
		return errors.New("service lease meter time moved backward")
	}
	if at.After(lease.ExpiresAt) {
		at = lease.ExpiresAt
	}
	if !at.After(lease.LastMeteredAt) {
		return nil
	}
	elapsed := at.Sub(lease.LastMeteredAt).Nanoseconds()
	add, err := mulDiv(int64(lease.ActiveReplicas), elapsed, 1, false)
	if err != nil || add < 0 || lease.CumulativeReplicaNanos > int64(^uint64(0)>>1)-add {
		return errors.New("service lease replica-time overflow")
	}
	lease.CumulativeReplicaNanos += add
	// A zero-replica window (FAILOVER_REQUIRED, drained) with no prior accrual
	// must advance the watermark without inventing money. ServiceLeaseMoney
	// requires a positive aggregate duration; calling it with zero fails closed
	// and would block FailoverServiceLease / termination for a lease that died
	// before its first billable interval.
	if lease.CumulativeReplicaNanos <= 0 {
		lease.LastMeteredAt = at
		return nil
	}
	money, err := ServiceLeaseMoneyForReplicaDuration(MustParseCurrency(lease.Pricing.Currency), *lease.Pricing.ServiceLease, lease.CumulativeReplicaNanos)
	if err != nil {
		return err
	}
	variable, err := serviceLeaseKnownVariableNanos(*lease.Pricing.ServiceLease, money)
	if err != nil || money.BuyerCharge.Nanos > lease.Pricing.FixedPoint.AcceptedCeilingNanos {
		return errors.New("service lease meter violates reserved ceiling or exact cost bounds")
	}
	var sequence int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(sequence),0)+1 FROM service_lease_meterings WHERE lease_id=$1`, lease.ID).Scan(&sequence); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO service_lease_meterings
		(lease_id,sequence,metered_at,cumulative_replica_nanoseconds,buyer_charge_nanos,supplier_payable_nanos,known_variable_cost_nanos,known_contribution_nanos)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, lease.ID, sequence, at, lease.CumulativeReplicaNanos,
		money.BuyerCharge.Nanos, money.SupplierPayable.Nanos, variable, money.MercContribution.Nanos); err != nil {
		return err
	}
	supplierDelta := money.SupplierPayable.Nanos - lease.SupplierPayableNanos
	if supplierDelta < 0 {
		return errors.New("service lease supplier payable moved backward")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO service_lease_supplier_meterings
		(lease_id,sequence,supplier_id,supplier_payable_delta_nanos)
		VALUES ($1,$2,$3,$4)`, lease.ID, sequence, lease.SupplierID, supplierDelta); err != nil {
		return err
	}
	lease.LastMeteredAt, lease.BuyerChargeNanos, lease.SupplierPayableNanos = at, money.BuyerCharge.Nanos, money.SupplierPayable.Nanos
	lease.KnownVariableCostNanos, lease.KnownContributionNanos = variable, money.MercContribution.Nanos
	if _, err := tx.Exec(ctx, `INSERT INTO service_lease_events (lease_id,kind,detail)
		VALUES ($1,'METERED',jsonb_build_object('sequence',$2::bigint,'cumulative_replica_nanoseconds',$3::bigint,'buyer_charge_nanos',$4::bigint,'supplier_payable_nanos',$5::bigint))`,
		lease.ID, sequence, lease.CumulativeReplicaNanos, lease.BuyerChargeNanos, lease.SupplierPayableNanos); err != nil {
		return err
	}
	return nil
}

func (s *Store) HeartbeatServiceLease(ctx context.Context, auth WorkerAuth, leaseID uuid.UUID, heartbeat ServiceLeaseHeartbeat) error {
	if leaseID == uuid.Nil || heartbeat.WarmReplicas < 0 || heartbeat.P95LatencyMillis < 0 ||
		heartbeat.LatencyMeasurementCount < 0 || heartbeat.LatencyWindowSeconds < 0 {
		return errors.New("invalid service lease heartbeat")
	}
	switch heartbeat.Status {
	case "READY", "DRAINING", "FAILED":
	default:
		return errors.New("invalid service lease heartbeat status")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	lease, err := scanServiceLease(tx.QueryRow(ctx, `SELECT `+serviceLeaseColumns+` FROM service_leases WHERE id=$1 FOR UPDATE`, leaseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil || lease.WorkerID != auth.WorkerID || lease.SupplierID != auth.SupplierID || lease.State == "COMPLETED" || lease.State == "CANCELLED" {
		if err != nil {
			return err
		}
		return errors.New("worker is not authorized to meter this active service lease")
	}
	now := time.Now().UTC()
	if err := meterServiceLeaseTx(ctx, tx, &lease, now); err != nil {
		return err
	}
	if heartbeat.Status == "READY" && (heartbeat.WarmReplicas < lease.MinimumReplicas || heartbeat.WarmReplicas > lease.MaximumReplicas || heartbeat.P95LatencyMillis > lease.MaximumP95LatencyMillis) {
		return errors.New("service lease heartbeat violates reserved replica or latency SLO")
	}
	if heartbeat.Status == "READY" && (heartbeat.LatencyMeasurementKind != "DATA_PLANE_COMPLETIONS_V1" ||
		heartbeat.LatencyMeasurementCount < 5 || heartbeat.LatencyWindowSeconds < 1 || heartbeat.LatencyWindowSeconds > 300) {
		return errors.New("ready service lease heartbeat requires a recent five-sample data-plane latency measurement")
	}
	if heartbeat.Status == "READY" && !validSHA256(heartbeat.DataPlaneProbeReceiptSHA256) {
		return errors.New("ready service lease heartbeat requires the bounded data-plane probe receipt digest")
	}
	if heartbeat.Status != "READY" && heartbeat.DataPlaneProbeReceiptSHA256 != "" {
		return errors.New("only a ready service lease heartbeat may carry a data-plane probe receipt digest")
	}
	nextState, nextReplicas := lease.State, heartbeat.WarmReplicas
	switch heartbeat.Status {
	case "READY":
		if _, err := tx.Exec(ctx, `INSERT INTO service_lease_events (lease_id,kind,detail)
			VALUES ($1,'SLO_MEASURED',jsonb_build_object(
			 'p95_latency_milliseconds',$2::bigint,
			 'latency_measurement_count',$3::int,
			 'latency_window_seconds',$4::bigint,
			 'latency_measurement_kind',$5::text,
			 'data_plane_probe_receipt_sha256',$6::text))`,
			lease.ID, heartbeat.P95LatencyMillis, heartbeat.LatencyMeasurementCount,
			heartbeat.LatencyWindowSeconds, heartbeat.LatencyMeasurementKind,
			heartbeat.DataPlaneProbeReceiptSHA256); err != nil {
			return err
		}
		if lease.State == "UPGRADING" {
			if heartbeat.UpgradeGeneration == "" || heartbeat.UpgradeGeneration == lease.UpgradeGeneration {
				return errors.New("rolling upgrade completion requires a new generation")
			}
			nextState = "ACTIVE"
			if _, err := tx.Exec(ctx, `INSERT INTO service_lease_events (lease_id,kind,detail) VALUES ($1,'ROLLING_UPDATE_COMPLETED',jsonb_build_object('generation',$2::text))`, lease.ID, heartbeat.UpgradeGeneration); err != nil {
				return err
			}
		}
	case "DRAINING":
		if !lease.Pricing.ServiceLease.MinimumReplicasIsOneOrMore() || heartbeat.UpgradeGeneration == "" {
			return errors.New("rolling update requires a non-empty generation")
		}
		nextState, nextReplicas = "UPGRADING", lease.MinimumReplicas
		if _, err := tx.Exec(ctx, `INSERT INTO service_lease_events (lease_id,kind,detail) VALUES ($1,'ROLLING_UPDATE_STARTED',jsonb_build_object('generation',$2::text))`, lease.ID, heartbeat.UpgradeGeneration); err != nil {
			return err
		}
	case "FAILED":
		nextState, nextReplicas = "FAILOVER_REQUIRED", 0
		if _, err := tx.Exec(ctx, `INSERT INTO service_lease_events (lease_id,kind,detail) VALUES ($1,'WORKER_LOSS',jsonb_build_object('reported_by','worker'))`, lease.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE service_leases SET state=$2,active_replicas=$3,upgrade_generation=$4,last_worker_heartbeat_at=$5,
		last_metered_at=$6,cumulative_replica_nanoseconds=$7,buyer_charge_nanos=$8,supplier_payable_nanos=$9,
		known_variable_cost_nanos=$10,known_contribution_nanos=$11 WHERE id=$1`,
		lease.ID, nextState, nextReplicas, heartbeat.UpgradeGeneration, now, lease.LastMeteredAt,
		lease.CumulativeReplicaNanos, lease.BuyerChargeNanos, lease.SupplierPayableNanos, lease.KnownVariableCostNanos, lease.KnownContributionNanos); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MinimumReplicasIsOneOrMore makes the upgrade predicate auditable at the
// authority boundary rather than relying on a coincidental database CHECK.
func (a ServiceLeasePricingAuthority) MinimumReplicasIsOneOrMore() bool {
	return a.MinimumReplicas >= 1
}

func (s *Store) GetServiceLeaseReceipt(ctx context.Context, buyerID, leaseID uuid.UUID) (ServiceLeaseReceipt, error) {
	lease, err := scanServiceLease(s.pool.QueryRow(ctx, `SELECT `+serviceLeaseColumns+` FROM service_leases WHERE id=$1 AND buyer_id=$2`, leaseID, buyerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ServiceLeaseReceipt{}, errNotFound
	}
	if err != nil {
		return ServiceLeaseReceipt{}, err
	}
	buyerFundingState := "LEGACY_UNFUNDED"
	supplierSettlementState := "ACCRUED_UNFUNDED"
	if lease.ReservedBuyerMicros > 0 {
		buyerFundingState = "PREPAID_MAXIMUM_RESERVED"
		supplierSettlementState = "ACCRUED_PREPAID_RESERVED_UNSETTLED"
	}
	residencyStatus := "SELECTED_SUPPLIER_ALL_IN_LIABILITY_BOUND_REGION_DECLARATION_NOT_CERTIFICATION"
	reserveRefundStatus := "UNCHARGED_LEASE_RESERVE_AND_GOVERNED_SLO_REFUND_UNDEFINED"
	if lease.Pricing.ServiceLease.Version == serviceLeasePricingAuthorityLegacyVersion {
		residencyStatus = "LEGACY_RESIDENCY_LIABILITY_BENEFICIARY_UNBOUND"
		reserveRefundStatus = "LEGACY_MODELED_RESERVE_CHARGED_WITHOUT_LIFECYCLE_OR_GOVERNED_REFUND"
	}
	blockers := serviceLeaseEconomicFinalityBlockers()
	receipt := ServiceLeaseReceipt{Lease: lease, BuyerFundingState: buyerFundingState, SupplierSettlementState: supplierSettlementState,
		TrueNetContributionStatus: "UNKNOWN_ECONOMIC_FINALITY_BLOCKERS", DataPlaneAuthorityStatus: "WORKER_ATTESTED_PROBE_NOT_BUYER_REQUEST",
		ResidencyAuthorityStatus: residencyStatus,
		EgressAuthorityStatus:    "APPLICATION_BYTES_DIAGNOSTIC_ONLY_PROVIDER_BILLING_UNKNOWN",
		ReserveRefundStatus:      reserveRefundStatus,
		ReceiptBlockers:          blockers,
		MeteringSemantics:        "cumulative replica-nanoseconds; each receipt is re-derived from lease start"}
	var activationRaw []byte
	err = s.pool.QueryRow(ctx, `SELECT detail FROM service_lease_events
		WHERE lease_id=$1 AND kind='ACTIVATED' ORDER BY created_at,id LIMIT 1`, lease.ID).Scan(&activationRaw)
	if err == nil {
		var activation serviceLeaseActivationDetail
		if err := json.Unmarshal(activationRaw, &activation); err != nil {
			return ServiceLeaseReceipt{}, err
		}
		receipt.MarketClearing = activation.MarketClearing
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ServiceLeaseReceipt{}, err
	}
	if (lease.State == "COMPLETED" || lease.State == "CANCELLED") && lease.ReservedBuyerMicros > 0 {
		settlement, serr := s.serviceLeaseTerminalSettlement(ctx, lease)
		if serr != nil {
			return ServiceLeaseReceipt{}, serr
		}
		if settlement == nil {
			// A row can exist from before terminal ledger settlement was added, or
			// can have been cancelled before its first billable meter interval.
			// Do not retroactively debit it: no stored cash fact proves that was
			// still the buyer's balance. The receipt says exactly what is missing.
			if lease.State == "CANCELLED" {
				receipt.BuyerFundingState = "PREPAID_RESERVATION_RELEASED_NO_METERED_USAGE"
				receipt.SupplierSettlementState = "NO_SUPPLIER_CREDIT_CANCELLED_NO_METERED_USAGE"
			} else {
				receipt.BuyerFundingState = "PREPAID_TERMINAL_SETTLEMENT_MISSING_LEGACY"
				receipt.SupplierSettlementState = "ACCRUED_PREPAID_RESERVED_UNSETTLED"
			}
		} else {
			receipt.Settlement = settlement
			receipt.BuyerFundingState = "PREPAID_FINAL_DEBIT_RECORDED"
			if settlement.FundingAuthorityState == "PREPAID_CASH_ALLOCATED_TO_SUPPLIER_LIABILITIES" {
				switch settlement.SupplierPayoutStatus {
				case PayoutSending:
					receipt.SupplierSettlementState = "SUPPLIER_CREDIT_FUNDED_PAYOUT_SENDING"
				case PayoutReleased, PayoutExported:
					receipt.SupplierSettlementState = "SUPPLIER_CREDIT_FUNDED_PAYOUT_" + strings.ToUpper(settlement.SupplierPayoutStatus)
				default:
					receipt.SupplierSettlementState = "SUPPLIER_CREDIT_FUNDED_TERMINAL_STATUS_" + settlement.SupplierPayoutStatus
				}
			} else {
				switch settlement.SupplierPayoutStatus {
				case PayoutHeld:
					receipt.SupplierSettlementState = "SUPPLIER_CREDIT_HELD_PREPAID_COLLECTION_ALLOCATION_REQUIRED"
				case PayoutAwaitingFunding:
					receipt.SupplierSettlementState = "SUPPLIER_CREDIT_AWAITING_PREPAID_COLLECTION_ALLOCATION"
				case PayoutCarried:
					receipt.SupplierSettlementState = "SUPPLIER_CREDIT_CARRIED_PENDING_PREPAID_COLLECTION_ALLOCATION"
				default:
					receipt.SupplierSettlementState = "SUPPLIER_CREDIT_TERMINAL_STATUS_" + settlement.SupplierPayoutStatus
				}
			}
		}
	}
	var evidence ServiceLeaseSLOEvidence
	err = s.pool.QueryRow(ctx, `SELECT
		(detail->>'p95_latency_milliseconds')::bigint,
		(detail->>'latency_measurement_count')::int,
		(detail->>'latency_window_seconds')::bigint,
		detail->>'latency_measurement_kind',
		COALESCE(detail->>'data_plane_probe_receipt_sha256',''),created_at
		FROM service_lease_events
		WHERE lease_id=$1 AND kind='SLO_MEASURED'
		ORDER BY created_at DESC,id DESC LIMIT 1`, lease.ID).
		Scan(&evidence.P95LatencyMillis, &evidence.LatencyMeasurementCount, &evidence.LatencyWindowSeconds,
			&evidence.LatencyMeasurementKind, &evidence.DataPlaneProbeReceiptSHA256, &evidence.MeasuredAt)
	if err == nil {
		receipt.LatestSLOEvidence = &evidence
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ServiceLeaseReceipt{}, err
	}
	var diagnostics ServiceLeaseDataPlaneDiagnostics
	if err := s.pool.QueryRow(ctx, `SELECT count(*)::bigint,
		COALESCE(sum(request_application_bytes),0)::bigint,
		COALESCE(sum(response_application_bytes),0)::bigint
		FROM service_lease_data_plane_diagnostics WHERE lease_id=$1`, lease.ID).
		Scan(&diagnostics.SuccessfulRequests, &diagnostics.RequestApplicationBytes,
			&diagnostics.ResponseApplicationBytes); err != nil {
		return ServiceLeaseReceipt{}, err
	}
	if diagnostics.SuccessfulRequests > 0 {
		diagnostics.AuthorityStatus = "APPLICATION_BYTES_DIAGNOSTIC_NOT_PROVIDER_BILLING"
		receipt.DataPlaneDiagnostics = &diagnostics
	}
	return receipt, nil
}

// serviceLeaseTerminalSettlement reassembles immutable buyer, prepaid,
// platform, and per-supplier entries written by settleFinalServiceLeaseTx. It
// refuses a partial or cross-currency set rather than displaying a receipt that
// looks paid while one component is missing. This query is deliberately
// independent of mutable lease aggregates.
func (s *Store) serviceLeaseTerminalSettlement(ctx context.Context, lease ServiceLease) (*ServiceLeaseSettlement, error) {
	payables, err := serviceLeaseSupplierPayables(ctx, s.pool, lease.ID)
	if err != nil {
		return nil, err
	}
	payableBySupplier := make(map[uuid.UUID]int64, len(payables))
	for _, payable := range payables {
		if payable.PayableNanos > 0 {
			payableBySupplier[payable.SupplierID] = payable.PayableNanos
		}
	}
	refs := []string{
		serviceLeaseLedgerRef(lease.ID, KindBuyerCharge),
		prepaidServiceLeaseDebitRef(lease.ID),
		serviceLeaseLedgerRef(lease.ID, KindPlatformTake),
	}
	for supplierID := range payableBySupplier {
		refs = append(refs, serviceLeaseSupplierCreditLedgerRef(lease.ID, supplierID))
	}
	rows, err := s.pool.Query(ctx, `SELECT kind,(amount_usd*1000000)::bigint,currency,payout_status,payout_ref,supplier_id
		FROM ledger_entries WHERE payout_ref=ANY($1)`, refs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type ledgerFact struct {
		amount   int64
		status   string
		ref      string
		supplier *uuid.UUID
	}
	facts := make(map[string]ledgerFact, len(refs))
	currency := ""
	for rows.Next() {
		var kind, rowCurrency, status, ref string
		var supplierID *uuid.UUID
		var amount int64
		if err := rows.Scan(&kind, &amount, &rowCurrency, &status, &ref, &supplierID); err != nil {
			return nil, err
		}
		if rowCurrency != lease.Pricing.Currency {
			return nil, fmt.Errorf("service lease %s terminal ledger currency %s differs from pricing currency %s", lease.ID, rowCurrency, lease.Pricing.Currency)
		}
		if currency == "" {
			currency = rowCurrency
		} else if currency != rowCurrency {
			return nil, fmt.Errorf("service lease %s terminal ledger mixes currencies", lease.ID)
		}
		key := kind + "\x00" + ref
		if _, exists := facts[key]; exists {
			return nil, fmt.Errorf("service lease %s terminal ledger repeats %s", lease.ID, key)
		}
		facts[key] = ledgerFact{amount: amount, status: status, ref: ref, supplier: supplierID}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(facts) == 0 {
		return nil, nil
	}
	buyer, buyerOK := facts[KindBuyerCharge+"\x00"+serviceLeaseLedgerRef(lease.ID, KindBuyerCharge)]
	debit, debitOK := facts[KindPrepaidDebit+"\x00"+prepaidServiceLeaseDebitRef(lease.ID)]
	platform, platformOK := facts[KindPlatformTake+"\x00"+serviceLeaseLedgerRef(lease.ID, KindPlatformTake)]
	if !buyerOK || !debitOK || !platformOK || currency == "" ||
		buyer.amount >= 0 || debit.amount >= 0 || platform.amount < 0 || buyer.amount != debit.amount {
		return nil, fmt.Errorf("service lease %s terminal ledger is incomplete or not conserved", lease.ID)
	}
	credits := make([]ServiceLeaseSupplierCredit, 0, len(payableBySupplier))
	var supplierMicros int64
	uniformStatus := ""
	for supplierID, payableNanos := range payableBySupplier {
		row, ok := facts[KindSupplierCredit+"\x00"+serviceLeaseSupplierCreditLedgerRef(lease.ID, supplierID)]
		if !ok || row.supplier == nil || *row.supplier != supplierID || row.amount <= 0 ||
			supplierMicros > int64(^uint64(0)>>1)-row.amount {
			return nil, fmt.Errorf("service lease %s terminal supplier ledger is incomplete", lease.ID)
		}
		supplierMicros += row.amount
		if uniformStatus == "" {
			uniformStatus = row.status
		} else if uniformStatus != row.status {
			uniformStatus = "MIXED"
		}
		credits = append(credits, ServiceLeaseSupplierCredit{
			SupplierID: supplierID, CreditMicros: row.amount, PayoutStatus: row.status, PayableNanos: payableNanos,
		})
	}
	if len(credits) == 0 || -buyer.amount != supplierMicros+platform.amount {
		return nil, fmt.Errorf("service lease %s terminal ledger is not conserved", lease.ID)
	}
	sort.Slice(credits, func(i, j int) bool { return credits[i].SupplierID.String() < credits[j].SupplierID.String() })
	fundingState := "PREPAID_CASH_COLLECTED_BUT_PAYOUT_COLLECTION_ALLOCATION_NOT_IMPLEMENTED"
	var fundedCredits int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*)::int
		  FROM supplier_payout_funding
		 WHERE source_kind='buyer_collection' AND liability_service_lease_id=$1
		   AND currency=$2`, lease.ID, currency).Scan(&fundedCredits); err != nil {
		return nil, err
	}
	if fundedCredits == len(credits) {
		fundingState = "PREPAID_CASH_ALLOCATED_TO_SUPPLIER_LIABILITIES"
	}
	blockers := serviceLeaseEconomicFinalityBlockers()
	moneyFinality, economicFinal := serviceLeaseMoneyTerminalFinality(blockers)
	// True-net remains refused on this lane while blockers are non-empty.
	if economicFinal {
		economicFinal = false
		moneyFinality = laneFinalityMoneyTerminalNotEconomicFinal
	}
	return &ServiceLeaseSettlement{
		Currency: currency, BuyerChargeMicros: -buyer.amount, PrepaidDebitMicros: -debit.amount,
		SupplierCreditMicros: supplierMicros, PlatformGrossMicros: platform.amount,
		SupplierPayoutStatus:  uniformStatus,
		FundingAuthorityState: fundingState,
		SupplierCredits:       credits,
		PricingDecisionSHA256: lease.PricingDecisionSHA256,
		MoneyFinalityStatus:   moneyFinality,
		EconomicFinalityStatus: func() string {
			if economicFinal {
				return laneFinalityEconomicFinal
			}
			return "UNKNOWN_ECONOMIC_FINALITY_BLOCKERS"
		}(),
		EconomicFinal:    economicFinal,
		FinalityBlockers: blockers,
	}, nil
}

func (s *Store) ListWorkerServiceLeaseAssignments(ctx context.Context, auth WorkerAuth) ([]ServiceLeaseAssignment, error) {
	if auth.WorkerID == uuid.Nil || auth.SupplierID == uuid.Nil {
		return nil, errors.New("service lease assignments require worker and supplier identity")
	}
	rows, err := s.pool.Query(ctx, `SELECT id,runtime_profile_id,region,minimum_replicas,maximum_replicas,
		maximum_p95_latency_milliseconds,state,upgrade_generation,expires_at
		FROM service_leases
		WHERE worker_id=$1 AND supplier_id=$2 AND state IN ('ACTIVE','UPGRADING') AND expires_at>now()
		ORDER BY expires_at,id`, auth.WorkerID, auth.SupplierID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assignments := make([]ServiceLeaseAssignment, 0)
	for rows.Next() {
		var assignment ServiceLeaseAssignment
		if err := rows.Scan(&assignment.ID, &assignment.RuntimeProfileID, &assignment.Region,
			&assignment.MinimumReplicas, &assignment.MaximumReplicas, &assignment.MaximumP95LatencyMillis,
			&assignment.State, &assignment.UpgradeGeneration, &assignment.ExpiresAt); err != nil {
			return nil, err
		}
		assignments = append(assignments, assignment)
	}
	return assignments, rows.Err()
}

// CancelServiceLease ends a buyer-owned reservation without billing an interval
// that the worker never authenticated. Before expiry it meters only through the
// latest authenticated heartbeat; once the frozen expiry has passed it follows
// the ordinary expiry rule and settles the full accepted term. The unused
// prepaid maximum is released simply by removing this terminal lease from the
// reservation query; it is never materialised as a synthetic refund.
func (s *Store) CancelServiceLease(ctx context.Context, buyerID, leaseID uuid.UUID) (ServiceLease, bool, error) {
	if buyerID == uuid.Nil || leaseID == uuid.Nil {
		return ServiceLease{}, false, errors.New("service lease cancellation requires buyer and lease identity")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ServiceLease{}, false, err
	}
	defer tx.Rollback(ctx)
	lease, err := scanServiceLease(tx.QueryRow(ctx, `SELECT `+serviceLeaseColumns+`
		FROM service_leases WHERE id=$1 AND buyer_id=$2 FOR UPDATE`, leaseID, buyerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ServiceLease{}, false, errNotFound
	}
	if err != nil {
		return ServiceLease{}, false, err
	}
	if lease.State == "COMPLETED" || lease.State == "CANCELLED" {
		if err := tx.Commit(ctx); err != nil {
			return ServiceLease{}, false, err
		}
		return lease, false, nil
	}

	now := time.Now().UTC()
	cutoff := lease.LastMeteredAt
	if !now.Before(lease.ExpiresAt) {
		// The buyer's cancel raced with a term that already ended. Preserve the
		// existing expiry semantics instead of granting an unearned post-term
		// refund.
		cutoff = lease.ExpiresAt
	} else if lease.State == "ACTIVE" || lease.State == "UPGRADING" {
		// A worker heartbeat authenticates the state only at its own timestamp;
		// never bill the quiet tail merely because the buyer chose to stop.
		cutoff = lease.LastWorkerHeartbeatAt
		if cutoff.After(now) {
			cutoff = now
		}
		if cutoff.Before(lease.LastMeteredAt) {
			cutoff = lease.LastMeteredAt
		}
	}
	if err := meterServiceLeaseTx(ctx, tx, &lease, cutoff); err != nil {
		return ServiceLease{}, false, err
	}
	if lease.BuyerChargeNanos > 0 {
		if err := settleFinalServiceLeaseTx(ctx, tx, &lease); err != nil {
			return ServiceLease{}, false, err
		}
	}
	usedMicros := int64(0)
	if lease.BuyerChargeNanos > 0 {
		currency, err := ParseCurrency(lease.Pricing.Currency)
		if err != nil {
			return ServiceLease{}, false, err
		}
		usedMicros, err = LedgerMicrosFromNanos(MoneyNanos{Currency: currency, Nanos: lease.BuyerChargeNanos})
		if err != nil {
			return ServiceLease{}, false, err
		}
	}
	if usedMicros < 0 || usedMicros > lease.ReservedBuyerMicros {
		return ServiceLease{}, false, errors.New("service lease cancellation violates frozen prepaid reservation")
	}
	if _, err := tx.Exec(ctx, `UPDATE service_leases SET state='CANCELLED',active_replicas=0,
		finalized_at=$2,last_metered_at=$3,cumulative_replica_nanoseconds=$4,
		buyer_charge_nanos=$5,supplier_payable_nanos=$6,known_variable_cost_nanos=$7,
		known_contribution_nanos=$8 WHERE id=$1`, lease.ID, now, lease.LastMeteredAt,
		lease.CumulativeReplicaNanos, lease.BuyerChargeNanos, lease.SupplierPayableNanos,
		lease.KnownVariableCostNanos, lease.KnownContributionNanos); err != nil {
		return ServiceLease{}, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE service_lease_worker_offers
		SET available_warm_replicas=LEAST(maximum_warm_replicas,available_warm_replicas+$4),updated_at=now()
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`, lease.WorkerID,
		lease.RuntimeProfileID, lease.Region, lease.MaximumReplicas); err != nil {
		return ServiceLease{}, false, err
	}
	if err := recordServiceLeaseOfferSampleTx(ctx, tx, lease.WorkerID, lease.RuntimeProfileID, lease.Region); err != nil {
		return ServiceLease{}, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO service_lease_events (lease_id,kind,detail)
		VALUES ($1,'CANCELLED',jsonb_build_object('metered_through',$2::timestamptz,
		'cumulative_replica_nanoseconds',$3::bigint,'buyer_charge_nanos',$4::bigint,
		'supplier_payable_nanos',$5::bigint,'unused_reserved_micros',$6::bigint))`,
		lease.ID, lease.LastMeteredAt, lease.CumulativeReplicaNanos, lease.BuyerChargeNanos,
		lease.SupplierPayableNanos, lease.ReservedBuyerMicros-usedMicros); err != nil {
		return ServiceLease{}, false, err
	}
	lease.State, lease.ActiveReplicas = "CANCELLED", 0
	lease.FinalizedAt = &now
	if err := tx.Commit(ctx); err != nil {
		return ServiceLease{}, false, err
	}
	return lease, true, nil
}

// RecoverServiceLeases turns a missing worker heartbeat into a fail-closed
// service state. It meters only through the last authenticated heartbeat, never
// through the later detector tick, so a lost worker does not generate invented
// supplier liability or buyer usage.
func (s *Store) RecoverServiceLeases(ctx context.Context, limit int) (int, error) {
	if limit < 1 {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM service_leases
		WHERE state IN ('ACTIVE','UPGRADING') AND last_worker_heartbeat_at < now()-$1::interval
		ORDER BY last_worker_heartbeat_at ASC LIMIT $2`, serviceLeaseHeartbeatTimeout.String(), limit)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	count := 0
	for _, id := range ids {
		changed, err := s.markServiceLeaseWorkerLost(ctx, id, "control_heartbeat_timeout")
		if err != nil {
			return count, err
		}
		if changed {
			count++
		}
	}
	return count, nil
}

func (s *Store) markServiceLeaseWorkerLost(ctx context.Context, leaseID uuid.UUID, source string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	lease, err := scanServiceLease(tx.QueryRow(ctx, `SELECT `+serviceLeaseColumns+` FROM service_leases WHERE id=$1 FOR UPDATE`, leaseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errNotFound
	}
	if err != nil {
		return false, err
	}
	if lease.State != "ACTIVE" && lease.State != "UPGRADING" {
		return false, nil
	}
	if err := meterServiceLeaseTx(ctx, tx, &lease, lease.LastWorkerHeartbeatAt); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE service_leases SET state='FAILOVER_REQUIRED',active_replicas=0,
		last_metered_at=$2,cumulative_replica_nanoseconds=$3,buyer_charge_nanos=$4,supplier_payable_nanos=$5,
		known_variable_cost_nanos=$6,known_contribution_nanos=$7 WHERE id=$1`, lease.ID, lease.LastMeteredAt,
		lease.CumulativeReplicaNanos, lease.BuyerChargeNanos, lease.SupplierPayableNanos, lease.KnownVariableCostNanos, lease.KnownContributionNanos); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO service_lease_events (lease_id,kind,detail)
		VALUES ($1,'WORKER_LOSS',jsonb_build_object('reported_by',$2::text))`, lease.ID, source); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// FailoverServiceLease is a control-plane recovery operation, not a
// buyer-selected supplier switch. The replacement must fit frozen region,
// capacity, and supplier/residency ceilings. Its work still settles at the
// original PricingDecision rates. This has no customer data-plane authority.
//
// Returns (false, nil) when the lease is not in FAILOVER_REQUIRED, has expired,
// or no replacement clears the frozen ceilings. Callers that must not leave
// money parked use FailoverPendingServiceLeases, which terminates on that miss.
func (s *Store) FailoverServiceLease(ctx context.Context, leaseID uuid.UUID) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	lease, err := scanServiceLease(tx.QueryRow(ctx, `SELECT `+serviceLeaseColumns+` FROM service_leases WHERE id=$1 FOR UPDATE`, leaseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errNotFound
	}
	if err != nil {
		return false, err
	}
	if lease.State != "FAILOVER_REQUIRED" || !time.Now().Before(lease.ExpiresAt) {
		return false, nil
	}
	authority := lease.Pricing.ServiceLease
	var workerID, supplierID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT worker_id,supplier_id FROM service_lease_worker_offers
		WHERE runtime_profile_id=$1 AND runtime_profile_sha256=$2 AND region=$3 AND status='READY'
		  AND p95_latency_milliseconds>0 AND latency_measurement_count>=5
		  AND latency_window_seconds BETWEEN 1 AND 300 AND latency_measurement_kind='DATA_PLANE_COMPLETIONS_V1'
		  AND p95_latency_milliseconds <= $8 AND worker_id<>$4 AND last_seen_at > now()-interval '45 seconds' AND available_warm_replicas >= $5
		  AND supplier_nanos_per_replica_hour <= $6 AND residency_nanos_per_replica_hour <= $7
		  AND currency=$9
		ORDER BY (supplier_nanos_per_replica_hour + residency_nanos_per_replica_hour),worker_id
		FOR UPDATE SKIP LOCKED LIMIT 1`,
		lease.RuntimeProfileID, lease.RuntimeProfileSHA256, lease.Region, lease.WorkerID, lease.MaximumReplicas,
		authority.SupplierNanosPerReplicaHour, authority.ResidencyNanosPerReplicaHour,
		lease.MaximumP95LatencyMillis, lease.Pricing.Currency).Scan(&workerID, &supplierID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	if err := meterServiceLeaseTx(ctx, tx, &lease, now); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE service_lease_worker_offers SET available_warm_replicas=available_warm_replicas-$4,updated_at=now()
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3 AND available_warm_replicas >= $4`, workerID, lease.RuntimeProfileID, lease.Region, lease.MaximumReplicas); err != nil {
		return false, err
	}
	if err := recordServiceLeaseOfferSampleTx(ctx, tx, workerID, lease.RuntimeProfileID, lease.Region); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE service_leases SET worker_id=$2,supplier_id=$3,state='ACTIVE',active_replicas=$4,
		last_metered_at=$5,last_worker_heartbeat_at=$5,cumulative_replica_nanoseconds=$6,buyer_charge_nanos=$7,
		supplier_payable_nanos=$8,known_variable_cost_nanos=$9,known_contribution_nanos=$10 WHERE id=$1`,
		lease.ID, workerID, supplierID, lease.MinimumReplicas, lease.LastMeteredAt, lease.CumulativeReplicaNanos,
		lease.BuyerChargeNanos, lease.SupplierPayableNanos, lease.KnownVariableCostNanos, lease.KnownContributionNanos); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO service_lease_events (lease_id,kind,detail)
		VALUES ($1,'FAILOVER_COMPLETED',jsonb_build_object('replacement_worker_id',$2::text,'path','replacement_found'))`, lease.ID, workerID.String()); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// FailoverPendingServiceLeases is the production recovery step after
// RecoverServiceLeases. For each FAILOVER_REQUIRED lease still inside its term
// it tries FailoverServiceLease; when no replacement clears the frozen ceilings
// it terminates the lease and releases the prepaid reservation so capacity that
// will never arrive cannot hold buyer funds until expires_at.
//
// Returns (failedOver, terminated, err). Each path records a service_lease_events
// row (FAILOVER_COMPLETED or FAILOVER_TERMINATED) so a receipt shows which ran.
func (s *Store) FailoverPendingServiceLeases(ctx context.Context, limit int) (int, int, error) {
	if limit < 1 {
		return 0, 0, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM service_leases
		WHERE state='FAILOVER_REQUIRED' AND expires_at>now()
		ORDER BY last_worker_heartbeat_at ASC LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return 0, 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	failedOver, terminated := 0, 0
	for _, id := range ids {
		moved, err := s.FailoverServiceLease(ctx, id)
		if err != nil {
			return failedOver, terminated, err
		}
		if moved {
			failedOver++
			continue
		}
		// No replacement under frozen ceilings (or a concurrent finalize/cancel
		// already cleared the row). Terminate releases the reservation rather
		// than spinning until expires_at.
		ok, err := s.TerminateServiceLeaseNoReplacement(ctx, id)
		if err != nil {
			return failedOver, terminated, err
		}
		if ok {
			terminated++
		}
	}
	return failedOver, terminated, nil
}

// TerminateServiceLeaseNoReplacement ends a FAILOVER_REQUIRED lease when no
// replacement clears the frozen ceilings. Billing stays at the last metered
// point (WORKER_LOSS already cut accrual at the final authenticated heartbeat);
// unused prepaid reservation is released by leaving the open-reservation set.
func (s *Store) TerminateServiceLeaseNoReplacement(ctx context.Context, leaseID uuid.UUID) (bool, error) {
	if leaseID == uuid.Nil {
		return false, errors.New("service lease termination requires lease identity")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	lease, err := scanServiceLease(tx.QueryRow(ctx, `SELECT `+serviceLeaseColumns+` FROM service_leases WHERE id=$1 FOR UPDATE`, leaseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errNotFound
	}
	if err != nil {
		return false, err
	}
	if lease.State != "FAILOVER_REQUIRED" {
		// Concurrent failover, cancel, or expiry already moved it.
		return false, nil
	}
	if !time.Now().Before(lease.ExpiresAt) {
		// Past term: ordinary expiry finalization owns the terminal receipt.
		return false, nil
	}
	// FAILOVER_REQUIRED already metered through the last heartbeat with
	// active_replicas=0. Re-metering at LastMeteredAt is a no-op for accrual and
	// keeps the settlement path identical to buyer cancel.
	if err := meterServiceLeaseTx(ctx, tx, &lease, lease.LastMeteredAt); err != nil {
		return false, err
	}
	if lease.BuyerChargeNanos > 0 {
		if err := settleFinalServiceLeaseTx(ctx, tx, &lease); err != nil {
			return false, err
		}
	}
	usedMicros := int64(0)
	if lease.BuyerChargeNanos > 0 {
		currency, err := ParseCurrency(lease.Pricing.Currency)
		if err != nil {
			return false, err
		}
		usedMicros, err = LedgerMicrosFromNanos(MoneyNanos{Currency: currency, Nanos: lease.BuyerChargeNanos})
		if err != nil {
			return false, err
		}
	}
	if usedMicros < 0 || usedMicros > lease.ReservedBuyerMicros {
		return false, errors.New("service lease failover termination violates frozen prepaid reservation")
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `UPDATE service_leases SET state='CANCELLED',active_replicas=0,
		finalized_at=$2,last_metered_at=$3,cumulative_replica_nanoseconds=$4,
		buyer_charge_nanos=$5,supplier_payable_nanos=$6,known_variable_cost_nanos=$7,
		known_contribution_nanos=$8 WHERE id=$1`, lease.ID, now, lease.LastMeteredAt,
		lease.CumulativeReplicaNanos, lease.BuyerChargeNanos, lease.SupplierPayableNanos,
		lease.KnownVariableCostNanos, lease.KnownContributionNanos); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE service_lease_worker_offers
		SET available_warm_replicas=LEAST(maximum_warm_replicas,available_warm_replicas+$4),updated_at=now()
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`, lease.WorkerID,
		lease.RuntimeProfileID, lease.Region, lease.MaximumReplicas); err != nil {
		return false, err
	}
	if err := recordServiceLeaseOfferSampleTx(ctx, tx, lease.WorkerID, lease.RuntimeProfileID, lease.Region); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO service_lease_events (lease_id,kind,detail)
		VALUES ($1,'FAILOVER_TERMINATED',jsonb_build_object(
			'path','no_replacement_under_frozen_ceiling',
			'reason','no_replacement_under_frozen_ceiling',
			'metered_through',$2::timestamptz,
			'cumulative_replica_nanoseconds',$3::bigint,
			'buyer_charge_nanos',$4::bigint,
			'supplier_payable_nanos',$5::bigint,
			'unused_reserved_micros',$6::bigint))`,
		lease.ID, lease.LastMeteredAt, lease.CumulativeReplicaNanos, lease.BuyerChargeNanos,
		lease.SupplierPayableNanos, lease.ReservedBuyerMicros-usedMicros); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *Store) FinalizeExpiredServiceLeases(ctx context.Context, limit int) (int, error) {
	if limit < 1 {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM service_leases WHERE state IN ('ACTIVE','UPGRADING','FAILOVER_REQUIRED') AND expires_at<=now() ORDER BY expires_at LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	completed := 0
	for _, id := range ids {
		ok, err := s.finalizeExpiredServiceLease(ctx, id)
		if err != nil {
			return completed, err
		}
		if ok {
			completed++
		}
	}
	return completed, nil
}

func (s *Store) finalizeExpiredServiceLease(ctx context.Context, leaseID uuid.UUID) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	lease, err := scanServiceLease(tx.QueryRow(ctx, `SELECT `+serviceLeaseColumns+` FROM service_leases WHERE id=$1 FOR UPDATE`, leaseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errNotFound
	}
	if err != nil {
		return false, err
	}
	if lease.State == "COMPLETED" || lease.State == "CANCELLED" || time.Now().Before(lease.ExpiresAt) {
		return false, nil
	}
	if err := meterServiceLeaseTx(ctx, tx, &lease, lease.ExpiresAt); err != nil {
		return false, err
	}
	if err := settleFinalServiceLeaseTx(ctx, tx, &lease); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE service_leases SET state='COMPLETED',finalized_at=now(),last_metered_at=$2,
		cumulative_replica_nanoseconds=$3,buyer_charge_nanos=$4,supplier_payable_nanos=$5,known_variable_cost_nanos=$6,known_contribution_nanos=$7 WHERE id=$1`,
		lease.ID, lease.LastMeteredAt, lease.CumulativeReplicaNanos, lease.BuyerChargeNanos, lease.SupplierPayableNanos, lease.KnownVariableCostNanos, lease.KnownContributionNanos); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE service_lease_worker_offers SET available_warm_replicas=LEAST(maximum_warm_replicas,available_warm_replicas+$4),updated_at=now()
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`, lease.WorkerID, lease.RuntimeProfileID, lease.Region, lease.MaximumReplicas); err != nil {
		return false, err
	}
	if err := recordServiceLeaseOfferSampleTx(ctx, tx, lease.WorkerID, lease.RuntimeProfileID, lease.Region); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO service_lease_events (lease_id,kind,detail) VALUES ($1,'EXPIRED',
		jsonb_build_object('buyer_charge_nanos',$2::bigint,'supplier_payable_nanos',$3::bigint,
		'final_prepaid_debit_ref',$4::text,'final_supplier_credit_ref_prefix',$5::text))`,
		lease.ID, lease.BuyerChargeNanos, lease.SupplierPayableNanos,
		prepaidServiceLeaseDebitRef(lease.ID), serviceLeaseLedgerRef(lease.ID, KindSupplierCredit)); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
