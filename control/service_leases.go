package main

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	serviceLeaseHeartbeatTimeout = 45 * time.Second
	serviceLeaseControlNanosHour = int64(100_000_000)
	serviceLeaseRiskNanosHour    = int64(100_000_000)
	serviceLeaseContributionHour = int64(300_000_000)
)

var serviceLeaseRegionPattern = regexp.MustCompile(`^[a-z0-9-]{2,64}$`)

type ServiceLeaseOfferRegistration struct {
	RuntimeProfileID             string `json:"runtime_profile_id"`
	RuntimeProfileSHA256         string `json:"runtime_profile_sha256"`
	Region                       string `json:"region"`
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
	Status                  string `json:"status"`
	UpgradeGeneration       string `json:"upgrade_generation,omitempty"`
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
	P95LatencyMillis        int64     `json:"p95_latency_milliseconds"`
	LatencyMeasurementCount int       `json:"latency_measurement_count"`
	LatencyWindowSeconds    int64     `json:"latency_window_seconds"`
	LatencyMeasurementKind  string    `json:"latency_measurement_kind"`
	MeasuredAt              time.Time `json:"measured_at"`
}

type ServiceLease struct {
	ID                      uuid.UUID       `json:"id"`
	BuyerID                 uuid.UUID       `json:"buyer_id"`
	WorkerID                uuid.UUID       `json:"worker_id"`
	SupplierID              uuid.UUID       `json:"supplier_id"`
	RuntimeProfileID        string          `json:"runtime_profile_id"`
	RuntimeProfileSHA256    string          `json:"runtime_profile_sha256"`
	Region                  string          `json:"region"`
	MinimumReplicas         int             `json:"minimum_replicas"`
	MaximumReplicas         int             `json:"maximum_replicas"`
	MaximumP95LatencyMillis int64           `json:"maximum_p95_latency_milliseconds"`
	TermSeconds             int64           `json:"term_seconds"`
	State                   string          `json:"state"`
	ActiveReplicas          int             `json:"active_replicas"`
	UpgradeGeneration       string          `json:"upgrade_generation,omitempty"`
	Pricing                 PricingDecision `json:"pricing_decision"`
	PricingDecisionSHA256   string          `json:"pricing_decision_sha256"`
	StartedAt               time.Time       `json:"started_at"`
	ExpiresAt               time.Time       `json:"expires_at"`
	LastMeteredAt           time.Time       `json:"last_metered_at"`
	LastWorkerHeartbeatAt   time.Time       `json:"last_worker_heartbeat_at"`
	CumulativeReplicaNanos  int64           `json:"cumulative_replica_nanoseconds"`
	BuyerChargeNanos        int64           `json:"buyer_charge_nanos"`
	SupplierPayableNanos    int64           `json:"supplier_payable_nanos"`
	KnownVariableCostNanos  int64           `json:"known_variable_cost_nanos"`
	KnownContributionNanos  int64           `json:"known_contribution_nanos"`
	FinalizedAt             *time.Time      `json:"finalized_at,omitempty"`
}

type ServiceLeaseReceipt struct {
	Lease                     ServiceLease             `json:"lease"`
	SupplierSettlementState   string                   `json:"supplier_settlement_state"`
	TrueNetContributionStatus string                   `json:"true_net_contribution_status"`
	DataPlaneAuthorityStatus  string                   `json:"data_plane_authority_status"`
	ResidencyAuthorityStatus  string                   `json:"residency_authority_status"`
	MeteringSemantics         string                   `json:"metering_semantics"`
	LatestSLOEvidence         *ServiceLeaseSLOEvidence `json:"latest_slo_evidence,omitempty"`
}

func validateServiceLeaseOffer(reg ServiceLeaseOfferRegistration) (VLLMRuntimeProfile, error) {
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
	if _, err := validateServiceLeaseOffer(reg); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO service_lease_worker_offers
		 (worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,region,
		  maximum_warm_replicas,available_warm_replicas,supplier_nanos_per_replica_hour,
		  residency_nanos_per_replica_hour,supports_rolling_upgrade,p95_latency_milliseconds,
		  latency_measurement_count,latency_window_seconds,latency_measurement_kind,status,last_seen_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,now(),now())
		ON CONFLICT (worker_id,runtime_profile_id,region) DO UPDATE SET
		 supplier_id=EXCLUDED.supplier_id,maximum_warm_replicas=EXCLUDED.maximum_warm_replicas,
		 available_warm_replicas=EXCLUDED.available_warm_replicas,
		 supplier_nanos_per_replica_hour=EXCLUDED.supplier_nanos_per_replica_hour,
		 residency_nanos_per_replica_hour=EXCLUDED.residency_nanos_per_replica_hour,
		 supports_rolling_upgrade=EXCLUDED.supports_rolling_upgrade,
		 p95_latency_milliseconds=EXCLUDED.p95_latency_milliseconds,
		 latency_measurement_count=EXCLUDED.latency_measurement_count,
		 latency_window_seconds=EXCLUDED.latency_window_seconds,
		 latency_measurement_kind=EXCLUDED.latency_measurement_kind,status=EXCLUDED.status,
		 last_seen_at=now(),updated_at=now()`,
		auth.WorkerID, auth.SupplierID, reg.RuntimeProfileID, reg.RuntimeProfileSHA256, reg.Region,
		reg.MaximumWarmReplicas, reg.AvailableWarmReplicas, reg.SupplierNanosPerReplicaHour,
		reg.ResidencyNanosPerReplicaHour, reg.SupportsRollingUpgrade, reg.P95LatencyMillis,
		reg.LatencyMeasurementCount, reg.LatencyWindowSeconds, reg.LatencyMeasurementKind, reg.Status)
	return err
}

func serviceLeasePricingInputs(profile VLLMRuntimeProfile, currency Currency, request ServiceLeaseRequest, supplierRate, residencyRate int64) ServiceLeasePricingInputs {
	return ServiceLeasePricingInputs{
		Profile: profile, Currency: currency, Region: request.Region,
		MinimumReplicas: request.MinimumReplicas, MaximumReplicas: request.MaximumReplicas,
		TermSeconds: request.TermSeconds, MaximumP95LatencyMilliseconds: request.MaximumP95LatencyMilliseconds,
		SupplierNanosPerReplicaHour: supplierRate, ResidencyNanosPerReplicaHour: residencyRate,
		ControlPlaneNanosPerReplicaHour: serviceLeaseControlNanosHour,
		RiskReserveNanosPerReplicaHour:  serviceLeaseRiskNanosHour,
		ContributionNanosPerReplicaHour: serviceLeaseContributionHour,
		BuyerDeclaredCeilingNanos:       request.BuyerDeclaredCeilingNanos,
	}
}

func (s *Store) CreateServiceLease(ctx context.Context, buyerID uuid.UUID, request ServiceLeaseRequest) (ServiceLease, error) {
	if buyerID == uuid.Nil || !serviceLeaseRegionPattern.MatchString(request.Region) || request.BuyerDeclaredCeilingNanos <= 0 {
		return ServiceLease{}, errors.New("service lease request has invalid buyer, region, or ceiling")
	}
	profile, ok := vllmProfileByID(request.RuntimeProfileID)
	if !ok {
		return ServiceLease{}, errors.New("unknown service lease runtime profile")
	}
	currency, err := SettlementCurrency()
	if err != nil {
		return ServiceLease{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ServiceLease{}, err
	}
	defer tx.Rollback(ctx)

	var workerID, supplierID uuid.UUID
	var supplierRate, residencyRate int64
	err = tx.QueryRow(ctx, `
		SELECT worker_id,supplier_id,supplier_nanos_per_replica_hour,residency_nanos_per_replica_hour
		  FROM service_lease_worker_offers
		 WHERE runtime_profile_id=$1 AND runtime_profile_sha256=$2 AND region=$3 AND status='READY'
		   AND p95_latency_milliseconds>0 AND latency_measurement_count>=5
		   AND latency_window_seconds BETWEEN 1 AND 300 AND latency_measurement_kind='DATA_PLANE_COMPLETIONS_V1'
		   AND p95_latency_milliseconds <= $5 AND last_seen_at > now()-interval '45 seconds' AND available_warm_replicas >= $4
		 ORDER BY supplier_nanos_per_replica_hour ASC,worker_id ASC
		 FOR UPDATE SKIP LOCKED LIMIT 1`, profile.RuntimeProfileID, profile.ProfileSHA256, request.Region, request.MaximumReplicas, request.MaximumP95LatencyMilliseconds).
		Scan(&workerID, &supplierID, &supplierRate, &residencyRate)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServiceLease{}, errRealtimeNoSupply
	}
	if err != nil {
		return ServiceLease{}, err
	}
	pricing, err := newServiceLeasePricingDecision(serviceLeasePricingInputs(profile, currency, request, supplierRate, residencyRate))
	if err != nil {
		return ServiceLease{}, err
	}
	reservedMicros, err := LedgerMicrosFromNanos(MoneyNanos{Currency: currency, Nanos: pricing.FixedPoint.AcceptedCeilingNanos})
	if err != nil {
		return ServiceLease{}, err
	}
	var freeMicros, spentMicros, batchReservedMicros, realtimeReservedMicros, serviceReservedMicros int64
	err = tx.QueryRow(ctx, `
		SELECT round(b.free_credit_usd*1000000)::bigint,
		       COALESCE((SELECT -sum((le.amount_usd*1000000)::bigint) FROM ledger_entries le
		                  WHERE le.buyer_id=b.id AND le.kind IN ('buyer_charge','buyer_refund')),0)::bigint,
		       COALESCE((SELECT sum(round(j.estimated_usd*1000000)::bigint) FROM jobs j
		                  WHERE j.buyer_id=b.id AND j.status IN ('queued','running','verifying')),0)::bigint,
		       COALESCE((SELECT sum(round(c.maximum_price_usd*1000000)::bigint) FROM execution_contracts c
		                  WHERE c.buyer_id=b.id AND c.state='EXECUTING'),0)::bigint,
		       COALESCE((SELECT sum(GREATEST(((l.pricing_decision #>> '{fixed_point,accepted_ceiling_nanos}')::bigint+500)/1000,1))
		                  FROM service_leases l WHERE l.buyer_id=b.id
		                    AND l.state IN ('ACTIVE','UPGRADING','FAILOVER_REQUIRED')),0)::bigint
		  FROM buyers b WHERE b.id=$1 AND b.deleted_at IS NULL FOR UPDATE`, buyerID).
		Scan(&freeMicros, &spentMicros, &batchReservedMicros, &realtimeReservedMicros, &serviceReservedMicros)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServiceLease{}, errNotFound
	}
	if err != nil {
		return ServiceLease{}, err
	}
	if freeMicros-spentMicros-batchReservedMicros-realtimeReservedMicros-serviceReservedMicros < reservedMicros {
		return ServiceLease{}, errRealtimeInsufficientFunds
	}
	pricingJSON, err := json.Marshal(pricing)
	if err != nil {
		return ServiceLease{}, err
	}
	pricingSHA, err := pricingDecisionDigest(pricing)
	if err != nil {
		return ServiceLease{}, err
	}
	now := time.Now().UTC()
	lease := ServiceLease{ID: uuid.New(), BuyerID: buyerID, WorkerID: workerID, SupplierID: supplierID,
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		Region: request.Region, MinimumReplicas: request.MinimumReplicas, MaximumReplicas: request.MaximumReplicas,
		MaximumP95LatencyMillis: request.MaximumP95LatencyMilliseconds, TermSeconds: request.TermSeconds,
		State: "ACTIVE", ActiveReplicas: request.MinimumReplicas, Pricing: pricing, PricingDecisionSHA256: pricingSHA,
		StartedAt: now, ExpiresAt: now.Add(time.Duration(request.TermSeconds) * time.Second),
		LastMeteredAt: now, LastWorkerHeartbeatAt: now}
	_, err = tx.Exec(ctx, `
		INSERT INTO service_leases
		 (id,buyer_id,worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,region,
		  minimum_replicas,maximum_replicas,maximum_p95_latency_milliseconds,term_seconds,state,
		  active_replicas,pricing_decision,pricing_decision_sha256,started_at,expires_at,last_metered_at,last_worker_heartbeat_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'ACTIVE',$8,$12,$13,$14,$15,$14,$14)`,
		lease.ID, lease.BuyerID, lease.WorkerID, lease.SupplierID, lease.RuntimeProfileID,
		lease.RuntimeProfileSHA256, lease.Region, lease.MinimumReplicas, lease.MaximumReplicas,
		lease.MaximumP95LatencyMillis, lease.TermSeconds, pricingJSON, pricingSHA, now, lease.ExpiresAt)
	if err != nil {
		return ServiceLease{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE service_lease_worker_offers
		SET available_warm_replicas=available_warm_replicas-$4,updated_at=now()
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3 AND available_warm_replicas >= $4`,
		workerID, profile.RuntimeProfileID, request.Region, request.MaximumReplicas); err != nil {
		return ServiceLease{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO service_lease_events (lease_id,kind,detail)
		VALUES ($1,'ACTIVATED',jsonb_build_object('reserved_ceiling_nanos',$2::bigint,'currency',$3::text))`,
		lease.ID, pricing.FixedPoint.AcceptedCeilingNanos, currency.Code()); err != nil {
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

func scanServiceLease(row pgx.Row) (ServiceLease, error) {
	var lease ServiceLease
	var raw []byte
	err := row.Scan(&lease.ID, &lease.BuyerID, &lease.WorkerID, &lease.SupplierID,
		&lease.RuntimeProfileID, &lease.RuntimeProfileSHA256, &lease.Region, &lease.MinimumReplicas,
		&lease.MaximumReplicas, &lease.MaximumP95LatencyMillis, &lease.TermSeconds, &lease.State,
		&lease.ActiveReplicas, &lease.UpgradeGeneration, &raw, &lease.PricingDecisionSHA256,
		&lease.StartedAt, &lease.ExpiresAt, &lease.LastMeteredAt, &lease.LastWorkerHeartbeatAt,
		&lease.CumulativeReplicaNanos, &lease.BuyerChargeNanos, &lease.SupplierPayableNanos,
		&lease.KnownVariableCostNanos, &lease.KnownContributionNanos, &lease.FinalizedAt)
	if err != nil {
		return ServiceLease{}, err
	}
	lease.Pricing, err = decodeServiceLeasePricing(raw, lease.PricingDecisionSHA256)
	return lease, err
}

const serviceLeaseColumns = `id,buyer_id,worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,
 region,minimum_replicas,maximum_replicas,maximum_p95_latency_milliseconds,term_seconds,state,
 active_replicas,upgrade_generation,pricing_decision,pricing_decision_sha256,started_at,expires_at,
 last_metered_at,last_worker_heartbeat_at,cumulative_replica_nanoseconds,buyer_charge_nanos,
 supplier_payable_nanos,known_variable_cost_nanos,known_contribution_nanos,finalized_at`

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
	money, err := ServiceLeaseMoneyForReplicaDuration(MustParseCurrency(lease.Pricing.Currency), *lease.Pricing.ServiceLease, lease.CumulativeReplicaNanos)
	if err != nil {
		return err
	}
	variable := money.ResidencyCost.Nanos + money.ControlPlaneCost.Nanos + money.RiskReserve.Nanos
	if variable < money.ResidencyCost.Nanos || variable < money.ControlPlaneCost.Nanos ||
		money.BuyerCharge.Nanos > lease.Pricing.FixedPoint.AcceptedCeilingNanos {
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
	nextState, nextReplicas := lease.State, heartbeat.WarmReplicas
	switch heartbeat.Status {
	case "READY":
		if _, err := tx.Exec(ctx, `INSERT INTO service_lease_events (lease_id,kind,detail)
			VALUES ($1,'SLO_MEASURED',jsonb_build_object(
			 'p95_latency_milliseconds',$2::bigint,
			 'latency_measurement_count',$3::int,
			 'latency_window_seconds',$4::bigint,
			 'latency_measurement_kind',$5::text))`,
			lease.ID, heartbeat.P95LatencyMillis, heartbeat.LatencyMeasurementCount,
			heartbeat.LatencyWindowSeconds, heartbeat.LatencyMeasurementKind); err != nil {
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
	receipt := ServiceLeaseReceipt{Lease: lease, SupplierSettlementState: "ACCRUED_UNFUNDED",
		TrueNetContributionStatus: "UNKNOWN_PROCESSOR_FEE_UNALLOCATED", DataPlaneAuthorityStatus: "NOT_PROVEN_BY_CONTROL_PLANE",
		ResidencyAuthorityStatus: "SUPPLIER_DECLARED_OPERATIONAL_REGION_ONLY",
		MeteringSemantics:        "cumulative replica-nanoseconds; each receipt is re-derived from lease start"}
	var evidence ServiceLeaseSLOEvidence
	err = s.pool.QueryRow(ctx, `SELECT
		(detail->>'p95_latency_milliseconds')::bigint,
		(detail->>'latency_measurement_count')::int,
		(detail->>'latency_window_seconds')::bigint,
		detail->>'latency_measurement_kind',created_at
		FROM service_lease_events
		WHERE lease_id=$1 AND kind='SLO_MEASURED'
		ORDER BY created_at DESC,id DESC LIMIT 1`, lease.ID).
		Scan(&evidence.P95LatencyMillis, &evidence.LatencyMeasurementCount, &evidence.LatencyWindowSeconds,
			&evidence.LatencyMeasurementKind, &evidence.MeasuredAt)
	if err == nil {
		receipt.LatestSLOEvidence = &evidence
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ServiceLeaseReceipt{}, err
	}
	return receipt, nil
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
		ORDER BY supplier_nanos_per_replica_hour,worker_id FOR UPDATE SKIP LOCKED LIMIT 1`,
		lease.RuntimeProfileID, lease.RuntimeProfileSHA256, lease.Region, lease.WorkerID, lease.MaximumReplicas,
		authority.SupplierNanosPerReplicaHour, authority.ResidencyNanosPerReplicaHour, lease.MaximumP95LatencyMillis).Scan(&workerID, &supplierID)
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
	if _, err := tx.Exec(ctx, `UPDATE service_leases SET worker_id=$2,supplier_id=$3,state='ACTIVE',active_replicas=$4,
		last_metered_at=$5,last_worker_heartbeat_at=$5,cumulative_replica_nanoseconds=$6,buyer_charge_nanos=$7,
		supplier_payable_nanos=$8,known_variable_cost_nanos=$9,known_contribution_nanos=$10 WHERE id=$1`,
		lease.ID, workerID, supplierID, lease.MinimumReplicas, lease.LastMeteredAt, lease.CumulativeReplicaNanos,
		lease.BuyerChargeNanos, lease.SupplierPayableNanos, lease.KnownVariableCostNanos, lease.KnownContributionNanos); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO service_lease_events (lease_id,kind,detail)
		VALUES ($1,'FAILOVER_COMPLETED',jsonb_build_object('replacement_worker_id',$2::text))`, lease.ID, workerID.String()); err != nil {
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
	if _, err := tx.Exec(ctx, `UPDATE service_leases SET state='COMPLETED',finalized_at=now(),last_metered_at=$2,
		cumulative_replica_nanoseconds=$3,buyer_charge_nanos=$4,supplier_payable_nanos=$5,known_variable_cost_nanos=$6,known_contribution_nanos=$7 WHERE id=$1`,
		lease.ID, lease.LastMeteredAt, lease.CumulativeReplicaNanos, lease.BuyerChargeNanos, lease.SupplierPayableNanos, lease.KnownVariableCostNanos, lease.KnownContributionNanos); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE service_lease_worker_offers SET available_warm_replicas=LEAST(maximum_warm_replicas,available_warm_replicas+$4),updated_at=now()
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`, lease.WorkerID, lease.RuntimeProfileID, lease.Region, lease.MaximumReplicas); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO service_lease_events (lease_id,kind,detail) VALUES ($1,'EXPIRED','{}'::jsonb)`, lease.ID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
