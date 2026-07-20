package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrVerificationWorkConflict    = errors.New("verification work conflicts with its immutable attempt")
	ErrVerificationLeaseLost       = errors.New("verification work lease was lost")
	ErrVerificationWorkBusy        = errors.New("verification work is leased by another processor")
	ErrVerificationWorkTerminal    = errors.New("verification work is already terminal")
	ErrVerificationArtifactMissing = errors.New("verification artifact has not been pinned")
)

const (
	VerificationWorkPending  = "pending"
	VerificationWorkLeased   = "leased"
	VerificationWorkTerminal = "terminal"

	verificationSnapshotMaxBytes = 64 << 10
	verificationWorkMaxClaim     = 100
	verificationWorkErrorMax     = 1000
)

type VerificationWorkSnapshot struct {
	TaskID               uuid.UUID
	Attempt              int64
	JobID                uuid.UUID
	WorkerID             uuid.UUID
	SupplierID           uuid.UUID
	SnapshotVersion      int16
	Snapshot             json.RawMessage
	StagedResultKey      string
	ReportedResultSHA256 string
	DurationMS           int64
	TokensUsed           int64
	HardwareTempC        *float32
}

type VerificationArtifact struct {
	Key    string
	SHA256 string
	Bytes  int64
}

type VerificationWork struct {
	ID                  uuid.UUID
	Snapshot            VerificationWorkSnapshot
	SnapshotSHA256      string
	Status              string
	Artifact            *VerificationArtifact
	SamplingPolicy      string
	SamplingProbability *float64
	SamplingSelected    *bool
	LeaseAttempts       int
	NextAttemptAt       time.Time
	LastError           string
	TerminalOutcome     string
	DecisionSHA256      string
	TerminalAt          *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type VerificationLease struct {
	WorkID    uuid.UUID
	Owner     string
	Token     uuid.UUID
	ExpiresAt time.Time
}

type LeasedVerificationWork struct {
	Work  VerificationWork
	Lease VerificationLease
}

type verificationRowScanner interface {
	Scan(dest ...any) error
}

const verificationWorkColumns = `
 id,task_id,attempt,job_id,worker_id,supplier_id,
 snapshot_version,input_snapshot,snapshot_sha256,
 staged_result_key,COALESCE(reported_result_sha256,''),duration_ms,tokens_used,hardware_temp_c,
 sampling_policy,sampling_probability,sampling_selected,
 status,artifact_key,artifact_sha256,artifact_bytes,
 lease_owner,lease_token,lease_expires_at,lease_attempts,next_attempt_at,COALESCE(last_error,''),
 COALESCE(terminal_outcome,''),COALESCE(decision_sha256,''),terminal_at,created_at,updated_at`

func normalizeVerificationSHA(raw string, optional bool) (string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" && optional {
		return "", nil
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("sha256 must be 64 lowercase hexadecimal characters")
	}
	return raw, nil
}

func canonicalVerificationSnapshot(raw json.RawMessage) ([]byte, string, error) {
	if len(raw) == 0 || len(raw) > verificationSnapshotMaxBytes {
		return nil, "", fmt.Errorf("verification snapshot must be a non-empty object no larger than %d bytes", verificationSnapshotMaxBytes)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return nil, "", fmt.Errorf("verification snapshot: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value map[string]any
	if err := dec.Decode(&value); err != nil {
		return nil, "", fmt.Errorf("verification snapshot: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, "", fmt.Errorf("verification snapshot: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return canonical, hex.EncodeToString(sum[:]), nil
}

func prepareVerificationSnapshot(in VerificationWorkSnapshot) (VerificationWorkSnapshot, []byte, string, error) {
	if in.TaskID == uuid.Nil || in.JobID == uuid.Nil || in.WorkerID == uuid.Nil || in.SupplierID == uuid.Nil {
		return in, nil, "", errors.New("verification work requires task, job, worker, and supplier ids")
	}
	if in.Attempt < 0 {
		return in, nil, "", errors.New("verification work attempt must be non-negative")
	}
	if in.SnapshotVersion <= 0 {
		return in, nil, "", errors.New("verification snapshot version must be positive")
	}
	in.StagedResultKey = strings.TrimSpace(in.StagedResultKey)
	if in.StagedResultKey == "" {
		return in, nil, "", errors.New("verification staging result key is required")
	}
	var err error
	in.ReportedResultSHA256, err = normalizeVerificationSHA(in.ReportedResultSHA256, true)
	if err != nil {
		return in, nil, "", fmt.Errorf("reported result %w", err)
	}
	if in.DurationMS < 0 || in.TokensUsed < 0 {
		return in, nil, "", errors.New("verification reported duration and tokens must be non-negative")
	}
	if in.HardwareTempC != nil && (math.IsNaN(float64(*in.HardwareTempC)) || math.IsInf(float64(*in.HardwareTempC), 0)) {
		return in, nil, "", errors.New("verification hardware temperature must be finite")
	}
	canonical, digest, err := canonicalVerificationSnapshot(in.Snapshot)
	if err != nil {
		return in, nil, "", err
	}
	in.Snapshot = append(json.RawMessage(nil), canonical...)
	return in, canonical, digest, nil
}

func createVerificationWorkTx(ctx context.Context, tx pgx.Tx, snapshot VerificationWorkSnapshot) error {
	snapshot, canonical, digest, err := prepareVerificationSnapshot(snapshot)
	if err != nil {
		return err
	}
	ct, err := tx.Exec(ctx, `
		INSERT INTO verification_work
		 (task_id,attempt,job_id,worker_id,supplier_id,snapshot_version,input_snapshot,snapshot_sha256,
		  staged_result_key,reported_result_sha256,duration_ms,tokens_used,hardware_temp_c)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11,$12,$13)
		ON CONFLICT (task_id,attempt) DO NOTHING`,
		snapshot.TaskID, snapshot.Attempt, snapshot.JobID, snapshot.WorkerID, snapshot.SupplierID,
		snapshot.SnapshotVersion, canonical, digest, snapshot.StagedResultKey,
		snapshot.ReportedResultSHA256, snapshot.DurationMS, snapshot.TokensUsed, snapshot.HardwareTempC)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 1 {
		return nil
	}
	var exact bool
	if err := tx.QueryRow(ctx, `
		SELECT job_id=$3 AND worker_id=$4 AND supplier_id=$5 AND snapshot_version=$6
		 AND input_snapshot=$7::jsonb AND snapshot_sha256=$8
		 AND staged_result_key=$9 AND COALESCE(reported_result_sha256,'')=$10
		 AND duration_ms=$11 AND tokens_used=$12 AND hardware_temp_c IS NOT DISTINCT FROM $13::real
		FROM verification_work WHERE task_id=$1 AND attempt=$2`,
		snapshot.TaskID, snapshot.Attempt, snapshot.JobID, snapshot.WorkerID, snapshot.SupplierID,
		snapshot.SnapshotVersion, canonical, digest, snapshot.StagedResultKey,
		snapshot.ReportedResultSHA256, snapshot.DurationMS, snapshot.TokensUsed, snapshot.HardwareTempC).
		Scan(&exact); err != nil {
		return err
	}
	if !exact {
		return ErrVerificationWorkConflict
	}
	return nil
}

func scanVerificationWork(row verificationRowScanner) (VerificationWork, error) {
	var out VerificationWork
	var input []byte
	var reported string
	var temp *float32
	var samplingPolicy, samplingProbability *string
	var samplingSelected *bool
	var artifactKey, artifactSHA *string
	var artifactBytes *int64
	var leaseOwner *string
	var leaseToken *uuid.UUID
	var leaseExpires *time.Time
	if err := row.Scan(
		&out.ID, &out.Snapshot.TaskID, &out.Snapshot.Attempt, &out.Snapshot.JobID,
		&out.Snapshot.WorkerID, &out.Snapshot.SupplierID, &out.Snapshot.SnapshotVersion,
		&input, &out.SnapshotSHA256, &out.Snapshot.StagedResultKey, &reported,
		&out.Snapshot.DurationMS, &out.Snapshot.TokensUsed, &temp,
		&samplingPolicy, &samplingProbability, &samplingSelected,
		&out.Status, &artifactKey, &artifactSHA, &artifactBytes,
		&leaseOwner, &leaseToken, &leaseExpires, &out.LeaseAttempts, &out.NextAttemptAt, &out.LastError,
		&out.TerminalOutcome, &out.DecisionSHA256, &out.TerminalAt, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return out, err
	}
	out.Snapshot.Snapshot = append(json.RawMessage(nil), input...)
	out.Snapshot.ReportedResultSHA256 = reported
	out.Snapshot.HardwareTempC = temp
	if samplingPolicy != nil || samplingProbability != nil || samplingSelected != nil {
		if samplingPolicy == nil || samplingProbability == nil || samplingSelected == nil {
			return out, ErrVerificationWorkConflict
		}
		probability, err := strconv.ParseFloat(*samplingProbability, 64)
		if err != nil || math.IsNaN(probability) || math.IsInf(probability, 0) || probability < 0 || probability > 1 {
			return out, ErrVerificationWorkConflict
		}
		out.SamplingPolicy = *samplingPolicy
		out.SamplingProbability = &probability
		out.SamplingSelected = samplingSelected
	}
	if artifactKey != nil && artifactSHA != nil && artifactBytes != nil {
		out.Artifact = &VerificationArtifact{Key: *artifactKey, SHA256: *artifactSHA, Bytes: *artifactBytes}
	}
	return out, nil
}

func (s *Store) PinVerificationSampling(ctx context.Context, lease VerificationLease, probability float64, selected bool) (bool, error) {
	if err := normalizeVerificationLease(lease); err != nil {
		return false, err
	}
	if math.IsNaN(probability) || math.IsInf(probability, 0) || probability < 0 || probability > 1 {
		return false, errors.New("verification sampling probability must be within [0,1]")
	}
	probabilityText := strconv.FormatFloat(probability, 'g', 17, 64)
	ct, err := s.pool.Exec(ctx, `
		UPDATE verification_work
		   SET sampling_policy=$4,sampling_probability=$5,sampling_selected=$6,updated_at=now()
		 WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_token=$3
		   AND lease_expires_at>now() AND sampling_policy IS NULL`,
		lease.WorkID, strings.TrimSpace(lease.Owner), lease.Token,
		verificationSamplingPolicy, probabilityText, selected)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 1 {
		return true, nil
	}
	var policy, storedProbability string
	var storedSelected bool
	var leaseExact bool
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(sampling_policy,''),COALESCE(sampling_probability,''),
		       COALESCE(sampling_selected,false),
		       status='leased' AND lease_owner=$2 AND lease_token=$3 AND lease_expires_at>now()
		  FROM verification_work WHERE id=$1`, lease.WorkID, strings.TrimSpace(lease.Owner), lease.Token).
		Scan(&policy, &storedProbability, &storedSelected, &leaseExact); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrVerificationLeaseLost
		}
		return false, err
	}
	if !leaseExact {
		return false, ErrVerificationLeaseLost
	}
	if policy == verificationSamplingPolicy && storedProbability == probabilityText && storedSelected == selected {
		return false, nil
	}
	return false, ErrVerificationWorkConflict
}

func (s *Store) verificationWorkByID(ctx context.Context, id uuid.UUID) (VerificationWork, error) {
	return scanVerificationWork(s.pool.QueryRow(ctx,
		`SELECT `+verificationWorkColumns+` FROM verification_work WHERE id=$1`, id))
}

func (s *Store) VerificationWorkForAttempt(ctx context.Context, taskID uuid.UUID, attempt int64) (VerificationWork, error) {
	return scanVerificationWork(s.pool.QueryRow(ctx,
		`SELECT `+verificationWorkColumns+` FROM verification_work WHERE task_id=$1 AND attempt=$2`, taskID, attempt))
}

func (s *Store) OwnsVerificationChunkPlanTurn(ctx context.Context, workID, jobID uuid.UUID, chunkIndex int) (bool, error) {
	if workID == uuid.Nil || jobID == uuid.Nil || chunkIndex < 0 {
		return false, errors.New("verification chunk plan turn requires work, job, and non-negative chunk")
	}
	var owner uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT w.id
		  FROM verification_work w
		  JOIN verification_work_plans p ON p.work_id=w.id
		  JOIN tasks t ON t.id=w.task_id
		 WHERE w.job_id=$1 AND COALESCE(t.chunk_index,0)=$2
		   AND w.status<>'terminal'
		 ORDER BY p.created_at,w.id
		 LIMIT 1`, jobID, chunkIndex).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return owner == workID, nil
}

func (s *Store) ExactTerminalVerificationCommit(ctx context.Context, taskID, workerID uuid.UUID, c TaskCommit) (bool, error) {
	if taskID == uuid.Nil || workerID == uuid.Nil || c.DurationMS > math.MaxInt64 || c.TokensUsed > math.MaxInt64 {
		return false, nil
	}
	reported, err := normalizeVerificationSHA(c.ResultSHA256, true)
	if err != nil {
		return false, nil
	}
	var staged, storedSHA string
	var durationMS, tokensUsed int64
	var temp *float32
	err = s.pool.QueryRow(ctx, `
		SELECT staged_result_key,COALESCE(reported_result_sha256,''),duration_ms,tokens_used,hardware_temp_c
		  FROM verification_work
		 WHERE task_id=$1 AND worker_id=$2 AND attempt=$3 AND status='terminal'
		 LIMIT 1`, taskID, workerID, c.Attempt).
		Scan(&staged, &storedSHA, &durationMS, &tokensUsed, &temp)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return staged == strings.TrimSpace(c.ResultKey) && storedSHA == reported &&
		durationMS == int64(c.DurationMS) && tokensUsed == int64(c.TokensUsed) &&
		optionalFloat32Equal(temp, c.HardwareTempC), nil
}

func optionalFloat32Equal(a, b *float32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (s *Store) CreateVerificationWork(ctx context.Context, snapshot VerificationWorkSnapshot) (work VerificationWork, created bool, err error) {
	snapshot, canonical, digest, err := prepareVerificationSnapshot(snapshot)
	if err != nil {
		return work, false, err
	}
	var id uuid.UUID
	err = s.pool.QueryRow(ctx, `
		INSERT INTO verification_work
		 (task_id,attempt,job_id,worker_id,supplier_id,snapshot_version,input_snapshot,snapshot_sha256,
		  staged_result_key,reported_result_sha256,duration_ms,tokens_used,hardware_temp_c)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11,$12,$13)
		ON CONFLICT (task_id,attempt) DO NOTHING
		RETURNING id`,
		snapshot.TaskID, snapshot.Attempt, snapshot.JobID, snapshot.WorkerID, snapshot.SupplierID,
		snapshot.SnapshotVersion, canonical, digest, snapshot.StagedResultKey,
		snapshot.ReportedResultSHA256, snapshot.DurationMS, snapshot.TokensUsed, snapshot.HardwareTempC,
	).Scan(&id)
	if err == nil {
		work, err = s.verificationWorkByID(ctx, id)
		return work, true, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return work, false, err
	}
	var exact bool
	err = s.pool.QueryRow(ctx, `
		SELECT id,
		 job_id=$3 AND worker_id=$4 AND supplier_id=$5 AND snapshot_version=$6
		 AND input_snapshot=$7::jsonb AND snapshot_sha256=$8
		 AND staged_result_key=$9 AND COALESCE(reported_result_sha256,'')=$10
		 AND duration_ms=$11 AND tokens_used=$12 AND hardware_temp_c IS NOT DISTINCT FROM $13::real
		FROM verification_work WHERE task_id=$1 AND attempt=$2`,
		snapshot.TaskID, snapshot.Attempt, snapshot.JobID, snapshot.WorkerID, snapshot.SupplierID,
		snapshot.SnapshotVersion, canonical, digest, snapshot.StagedResultKey,
		snapshot.ReportedResultSHA256, snapshot.DurationMS, snapshot.TokensUsed, snapshot.HardwareTempC,
	).Scan(&id, &exact)
	if err != nil {
		return work, false, err
	}
	if !exact {
		return work, false, ErrVerificationWorkConflict
	}
	work, err = s.verificationWorkByID(ctx, id)
	return work, false, err
}

func (s *Store) ClaimVerificationWork(ctx context.Context, owner string, lease time.Duration, limit int) ([]LeasedVerificationWork, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > 200 || lease <= 0 || limit <= 0 {
		return nil, errors.New("verification claim requires owner, positive lease, and positive limit")
	}
	if limit > verificationWorkMaxClaim {
		limit = verificationWorkMaxClaim
	}
	rows, err := s.pool.Query(ctx, `
		WITH candidates AS (
		 SELECT id FROM verification_work
		 WHERE (status='pending' AND next_attempt_at<=now())
		    OR (status='leased' AND lease_expires_at<=now())
		 ORDER BY CASE WHEN status='leased' THEN lease_expires_at ELSE next_attempt_at END,created_at,id
		 FOR UPDATE SKIP LOCKED LIMIT $3
		)
		UPDATE verification_work w
		 SET status='leased',lease_owner=$1,lease_token=gen_random_uuid(),
		     lease_expires_at=now()+make_interval(secs=>$2::double precision),
		     lease_attempts=w.lease_attempts+1,updated_at=now()
		FROM candidates c WHERE w.id=c.id
		RETURNING w.id,w.lease_token,w.lease_expires_at`, owner, lease.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type claimed struct {
		id      uuid.UUID
		token   uuid.UUID
		expires time.Time
	}
	var claimedRows []claimed
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.id, &c.token, &c.expires); err != nil {
			return nil, err
		}
		claimedRows = append(claimedRows, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	out := make([]LeasedVerificationWork, 0, len(claimedRows))
	for _, c := range claimedRows {
		work, err := s.verificationWorkByID(ctx, c.id)
		if err != nil {
			return nil, err
		}
		out = append(out, LeasedVerificationWork{Work: work, Lease: VerificationLease{
			WorkID: work.ID, Owner: owner, Token: c.token, ExpiresAt: c.expires,
		}})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Work.CreatedAt.Equal(out[j].Work.CreatedAt) {
			return out[i].Work.ID.String() < out[j].Work.ID.String()
		}
		return out[i].Work.CreatedAt.Before(out[j].Work.CreatedAt)
	})
	return out, nil
}

func (s *Store) ClaimVerificationWorkForAttempt(ctx context.Context, taskID uuid.UUID, attempt int64, owner string, lease time.Duration) (LeasedVerificationWork, error) {
	var out LeasedVerificationWork
	owner = strings.TrimSpace(owner)
	if taskID == uuid.Nil || attempt < 0 || owner == "" || len(owner) > 200 || lease <= 0 {
		return out, errors.New("verification attempt claim requires task, attempt, owner, and positive lease")
	}
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		UPDATE verification_work
		 SET status='leased',lease_owner=$3,lease_token=gen_random_uuid(),
		     lease_expires_at=now()+make_interval(secs=>$4::double precision),
		     lease_attempts=lease_attempts+1,updated_at=now()
		 WHERE task_id=$1 AND attempt=$2
		   AND ((status='pending')
		        OR (status='leased' AND lease_expires_at<=now()))
		 RETURNING id`, taskID, attempt, owner, lease.Seconds()).Scan(&id)
	if err == nil {
		out.Work, err = s.verificationWorkByID(ctx, id)
		if err != nil {
			return out, err
		}
		if err := s.pool.QueryRow(ctx, `SELECT lease_token,lease_expires_at FROM verification_work WHERE id=$1`, id).
			Scan(&out.Lease.Token, &out.Lease.ExpiresAt); err != nil {
			return out, err
		}
		out.Lease.WorkID, out.Lease.Owner = id, owner
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	work, err := s.VerificationWorkForAttempt(ctx, taskID, attempt)
	if err != nil {
		return out, err
	}
	if work.Status == VerificationWorkTerminal {
		return LeasedVerificationWork{Work: work}, ErrVerificationWorkTerminal
	}
	return LeasedVerificationWork{Work: work}, ErrVerificationWorkBusy
}

func normalizeVerificationLease(lease VerificationLease) error {
	if lease.WorkID == uuid.Nil || lease.Token == uuid.Nil || strings.TrimSpace(lease.Owner) == "" {
		return ErrVerificationLeaseLost
	}
	return nil
}

func (s *Store) RenewVerificationLease(ctx context.Context, lease VerificationLease, extension time.Duration) (VerificationLease, error) {
	if err := normalizeVerificationLease(lease); err != nil {
		return VerificationLease{}, err
	}
	if extension <= 0 {
		return VerificationLease{}, errors.New("verification lease renewal requires a positive extension")
	}
	lease.Owner = strings.TrimSpace(lease.Owner)
	err := s.pool.QueryRow(ctx, `
		UPDATE verification_work
		   SET lease_expires_at=now()+make_interval(secs=>$4::double precision),updated_at=now()
		 WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_token=$3
		   AND lease_expires_at>now()
		 RETURNING lease_expires_at`, lease.WorkID, lease.Owner, lease.Token, extension.Seconds()).
		Scan(&lease.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerificationLease{}, ErrVerificationLeaseLost
	}
	if err != nil {
		return VerificationLease{}, err
	}
	return lease, nil
}

func (s *Store) PinVerificationArtifact(ctx context.Context, lease VerificationLease, artifact VerificationArtifact) (bool, error) {
	if err := normalizeVerificationLease(lease); err != nil {
		return false, err
	}
	artifact.Key = strings.TrimSpace(artifact.Key)
	if artifact.Key == "" || artifact.Bytes < 0 {
		return false, errors.New("verification artifact requires key and non-negative byte count")
	}
	var err error
	artifact.SHA256, err = normalizeVerificationSHA(artifact.SHA256, false)
	if err != nil {
		return false, fmt.Errorf("artifact %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id, taskID uuid.UUID
	var attempt int64
	err = tx.QueryRow(ctx, `
		UPDATE verification_work SET artifact_key=$4,artifact_sha256=$5,artifact_bytes=$6,updated_at=now()
		 WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_token=$3
		   AND lease_expires_at>now() AND artifact_key IS NULL
		RETURNING id,task_id,attempt`, lease.WorkID, strings.TrimSpace(lease.Owner), lease.Token,
		artifact.Key, artifact.SHA256, artifact.Bytes).Scan(&id, &taskID, &attempt)
	if err == nil {
		ct, uerr := tx.Exec(ctx, `
			UPDATE tasks SET result_ref=$2,result_sha256=$3
			 WHERE id=$1 AND status='verifying' AND retry_count=$4`, taskID, artifact.Key, artifact.SHA256, attempt)
		if uerr != nil {
			return false, uerr
		}
		if ct.RowsAffected() != 1 {
			return false, ErrVerificationWorkConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var status string
	var owner *string
	var token *uuid.UUID
	var leaseLive bool
	var key, digest *string
	var size *int64
	err = tx.QueryRow(ctx, `
		SELECT status,lease_owner,lease_token,lease_expires_at>now(),artifact_key,artifact_sha256,artifact_bytes
		FROM verification_work WHERE id=$1`, lease.WorkID).
		Scan(&status, &owner, &token, &leaseLive, &key, &digest, &size)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrVerificationLeaseLost
		}
		return false, err
	}
	if status != VerificationWorkLeased || owner == nil || *owner != strings.TrimSpace(lease.Owner) ||
		token == nil || *token != lease.Token || !leaseLive {
		return false, ErrVerificationLeaseLost
	}
	if key != nil && digest != nil && size != nil {
		if *key == artifact.Key && *digest == artifact.SHA256 && *size == artifact.Bytes {
			var resultRef, resultSHA string
			if err := tx.QueryRow(ctx, `
				SELECT COALESCE(t.result_ref,''),COALESCE(t.result_sha256,'')
				  FROM tasks t JOIN verification_work w ON w.task_id=t.id WHERE w.id=$1`, lease.WorkID).
				Scan(&resultRef, &resultSHA); err != nil {
				return false, err
			}
			if resultRef != artifact.Key || resultSHA != artifact.SHA256 {
				return false, ErrVerificationWorkConflict
			}
			return false, nil
		}
		return false, ErrVerificationWorkConflict
	}
	return false, ErrVerificationWorkConflict
}

func (s *Store) ReleaseVerificationWork(ctx context.Context, lease VerificationLease, retryAt time.Time, cause string) error {
	if err := normalizeVerificationLease(lease); err != nil {
		return err
	}
	if retryAt.IsZero() {
		retryAt = time.Now()
	}
	cause = truncate(strings.TrimSpace(cause), verificationWorkErrorMax)
	ct, err := s.pool.Exec(ctx, `
		UPDATE verification_work
		 SET status='pending',lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,
		     next_attempt_at=$4,last_error=NULLIF($5,''),updated_at=now()
		 WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_token=$3
		   AND lease_expires_at>now()`, lease.WorkID, strings.TrimSpace(lease.Owner), lease.Token, retryAt, cause)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return ErrVerificationLeaseLost
	}
	return nil
}

func (s *Store) MarkVerificationWorkTerminal(ctx context.Context, lease VerificationLease, outcome VerifyOutcome, decisionSHA256 string) (bool, error) {
	if err := normalizeVerificationLease(lease); err != nil {
		return false, err
	}
	switch outcome {
	case OutcomePass, OutcomePassWithPenalty, OutcomeLossNoPayout, OutcomeFail:
	default:
		return false, fmt.Errorf("unsupported verification terminal outcome %q", outcome)
	}
	var err error
	decisionSHA256, err = normalizeVerificationSHA(decisionSHA256, false)
	if err != nil {
		return false, fmt.Errorf("decision %w", err)
	}
	var id uuid.UUID
	err = s.pool.QueryRow(ctx, `
		UPDATE verification_work
		 SET status='terminal',terminal_outcome=$4,decision_sha256=$5,terminal_at=now(),
		     lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,updated_at=now()
		 WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_token=$3
		   AND lease_expires_at>now() AND artifact_key IS NOT NULL AND sampling_policy IS NOT NULL
		RETURNING id`, lease.WorkID, strings.TrimSpace(lease.Owner), lease.Token,
		string(outcome), decisionSHA256).Scan(&id)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var status, existingOutcome, existingDigest string
	var artifactKey *string
	err = s.pool.QueryRow(ctx, `
		SELECT status,COALESCE(terminal_outcome,''),COALESCE(decision_sha256,''),artifact_key
		FROM verification_work WHERE id=$1`, lease.WorkID).
		Scan(&status, &existingOutcome, &existingDigest, &artifactKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrVerificationLeaseLost
		}
		return false, err
	}
	if status == VerificationWorkTerminal {
		if existingOutcome == string(outcome) && existingDigest == decisionSHA256 {
			return false, nil
		}
		return false, ErrVerificationWorkConflict
	}
	if artifactKey == nil {
		return false, ErrVerificationArtifactMissing
	}
	return false, ErrVerificationLeaseLost
}
