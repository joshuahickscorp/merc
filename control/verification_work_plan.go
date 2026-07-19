package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	verificationWorkPlanVersion int16 = 1
	verificationSamplingPolicy        = "hmac-reputation-v1"
)

type VerificationWorkPlan struct {
	WorkID              uuid.UUID
	SnapshotSHA256      string
	Artifact            VerificationArtifact
	SamplingPolicy      string
	SamplingProbability float64
	SamplingSelected    bool
	Decision            VerificationDecision
	Settlement          []LedgerEntry
	DecisionSHA256      string
	CreatedAt           time.Time
}

func (s *Store) VerificationWorkPlan(ctx context.Context, workID uuid.UUID) (VerificationWorkPlan, error) {
	var out VerificationWorkPlan
	var probability string
	var decisionJSON, settlementJSON []byte
	var version int16
	err := s.pool.QueryRow(ctx, `
		SELECT work_id,plan_version,snapshot_sha256,artifact_key,artifact_sha256,artifact_bytes,
		       sampling_policy,sampling_probability,sampling_selected,
		       decision_json,settlement_json,decision_sha256,created_at
		  FROM verification_work_plans WHERE work_id=$1`, workID).
		Scan(&out.WorkID, &version, &out.SnapshotSHA256, &out.Artifact.Key, &out.Artifact.SHA256,
			&out.Artifact.Bytes, &out.SamplingPolicy, &probability, &out.SamplingSelected,
			&decisionJSON, &settlementJSON, &out.DecisionSHA256, &out.CreatedAt)
	if err != nil {
		return out, err
	}
	if version != verificationWorkPlanVersion || out.SamplingPolicy != verificationSamplingPolicy {
		return out, fmt.Errorf("unsupported verification work plan v%d/%q", version, out.SamplingPolicy)
	}
	out.SamplingProbability, err = strconv.ParseFloat(probability, 64)
	if err != nil || math.IsNaN(out.SamplingProbability) || math.IsInf(out.SamplingProbability, 0) || out.SamplingProbability < 0 || out.SamplingProbability > 1 {
		return out, fmt.Errorf("invalid persisted verification sampling probability %q", probability)
	}
	if err := json.Unmarshal(decisionJSON, &out.Decision); err != nil {
		return out, fmt.Errorf("decode persisted verification decision: %w", err)
	}
	var durable []canonicalVerificationSettlement
	if err := json.Unmarshal(settlementJSON, &durable); err != nil {
		return out, fmt.Errorf("decode persisted verification settlement: %w", err)
	}
	out.Settlement, err = ledgerEntriesFromCanonical(durable)
	if err != nil {
		return out, err
	}
	got, err := verificationDecisionDigest(out.Decision, out.Settlement)
	if err != nil {
		return out, err
	}
	if got != out.DecisionSHA256 {
		return out, fmt.Errorf("persisted verification plan digest mismatch: got %s want %s", got, out.DecisionSHA256)
	}
	return out, nil
}

func (s *Store) PersistVerificationWorkPlan(ctx context.Context, lease VerificationLease, work VerificationWork, probability float64, selected bool, decision VerificationDecision, settlement []LedgerEntry) (VerificationWorkPlan, bool, error) {
	var out VerificationWorkPlan
	if err := normalizeVerificationLease(lease); err != nil {
		return out, false, err
	}
	if work.ID != lease.WorkID || work.Artifact == nil || math.IsNaN(probability) || math.IsInf(probability, 0) || probability < 0 || probability > 1 {
		return out, false, ErrVerificationWorkConflict
	}
	if work.SamplingPolicy != verificationSamplingPolicy || work.SamplingProbability == nil ||
		work.SamplingSelected == nil || *work.SamplingProbability != probability || *work.SamplingSelected != selected {
		return out, false, ErrVerificationWorkConflict
	}
	info, _, err := commitInfoFromVerificationWork(work)
	if err != nil {
		return out, false, err
	}
	info.ResultKey = work.Artifact.Key
	info.ResultSHA256 = work.Artifact.SHA256
	if err := validateVerificationDecisionShape(info, decision, settlement); err != nil {
		return out, false, err
	}
	digest, err := verificationDecisionDigest(decision, settlement)
	if err != nil {
		return out, false, err
	}
	decisionJSON, err := json.Marshal(decision)
	if err != nil {
		return out, false, err
	}
	canonicalSettlement := canonicalVerificationSettlements(settlement)
	settlementJSON, err := json.Marshal(canonicalSettlement)
	if err != nil {
		return out, false, err
	}
	probabilityText := strconv.FormatFloat(probability, 'g', 17, 64)
	var inserted uuid.UUID
	err = s.pool.QueryRow(ctx, `
		INSERT INTO verification_work_plans
		 (work_id,plan_version,snapshot_sha256,artifact_key,artifact_sha256,artifact_bytes,
		  sampling_policy,sampling_probability,sampling_selected,decision_json,settlement_json,decision_sha256)
		SELECT w.id,$4,w.snapshot_sha256,w.artifact_key,w.artifact_sha256,w.artifact_bytes,
		       $5,$6,$7,$8,$9,$10
		  FROM verification_work w
		 WHERE w.id=$1 AND w.status='leased' AND w.lease_owner=$2 AND w.lease_token=$3
		   AND w.lease_expires_at>now() AND w.artifact_key IS NOT NULL
		   AND w.snapshot_sha256=$11 AND w.artifact_key=$12 AND w.artifact_sha256=$13 AND w.artifact_bytes=$14
		ON CONFLICT (work_id) DO NOTHING RETURNING work_id`,
		lease.WorkID, strings.TrimSpace(lease.Owner), lease.Token, verificationWorkPlanVersion,
		verificationSamplingPolicy, probabilityText, selected, decisionJSON, settlementJSON, digest,
		work.SnapshotSHA256, work.Artifact.Key, work.Artifact.SHA256, work.Artifact.Bytes).Scan(&inserted)
	if err == nil {
		out, err = s.VerificationWorkPlan(ctx, inserted)
		return out, true, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, false, err
	}
	out, err = s.VerificationWorkPlan(ctx, lease.WorkID)
	if err == nil {
		if out.SnapshotSHA256 == work.SnapshotSHA256 && out.Artifact == *work.Artifact &&
			out.SamplingProbability == probability && out.SamplingSelected == selected && out.DecisionSHA256 == digest {
			return out, false, nil
		}
		return out, false, ErrVerificationWorkConflict
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, ErrVerificationLeaseLost
	}
	return out, false, err
}

func ledgerEntriesFromCanonical(rows []canonicalVerificationSettlement) ([]LedgerEntry, error) {
	out := make([]LedgerEntry, 0, len(rows))
	for _, row := range rows {
		amount, err := strconv.ParseFloat(row.AmountUSD, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid persisted settlement amount %q", row.AmountUSD)
		}
		entry := LedgerEntry{Kind: row.Kind, AmountUSD: amount, PayoutStatus: row.PayoutStatus}
		if entry.SupplierID, err = optionalUUIDFromString(row.SupplierID); err != nil {
			return nil, err
		}
		if entry.BuyerID, err = optionalUUIDFromString(row.BuyerID); err != nil {
			return nil, err
		}
		if entry.TaskID, err = optionalUUIDFromString(row.TaskID); err != nil {
			return nil, err
		}
		if row.ReleaseAt != "" {
			parsed, err := time.Parse(time.RFC3339Nano, row.ReleaseAt)
			if err != nil {
				return nil, fmt.Errorf("invalid persisted settlement release_at: %w", err)
			}
			entry.ReleaseAt = &parsed
		}
		out = append(out, entry)
	}
	return out, nil
}

func optionalUUIDFromString(raw string) (*uuid.UUID, error) {
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid persisted settlement uuid %q", raw)
	}
	return &id, nil
}
