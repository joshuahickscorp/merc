package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BuyerDSARExport is deliberately explicit. Authentication and webhook secret
// hashes never enter the export, while provider and ledger records needed to
// explain money movement remain visible to the data subject.
type BuyerDSARExport struct {
	SchemaVersion int             `json:"schema_version"`
	GeneratedAt   time.Time       `json:"generated_at"`
	Buyer         json.RawMessage `json:"buyer"`
	Jobs          json.RawMessage `json:"jobs"`
	Quotes        json.RawMessage `json:"quotes"`
	Tasks         json.RawMessage `json:"tasks"`
	Artifacts     json.RawMessage `json:"artifacts"`
	Payments      json.RawMessage `json:"payments"`
	Webhooks      json.RawMessage `json:"webhooks"`
	Disputes      json.RawMessage `json:"disputes"`
	AuthMetadata  json.RawMessage `json:"authentication_metadata"`
	Provenance    json.RawMessage `json:"provenance"`
}

func queryJSON(ctx context.Context, tx pgx.Tx, query string, args ...any) (json.RawMessage, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, query, args...).Scan(&raw); err != nil {
		return nil, err
	}
	if !json.Valid(raw) {
		return nil, errors.New("database returned invalid JSON")
	}
	return json.RawMessage(raw), nil
}

func (s *Store) ExportBuyerData(ctx context.Context, buyerID uuid.UUID) (BuyerDSARExport, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return BuyerDSARExport{}, err
	}
	defer tx.Rollback(ctx)
	var active bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM buyers WHERE id=$1 AND deleted_at IS NULL)`, buyerID).Scan(&active); err != nil {
		return BuyerDSARExport{}, err
	}
	if !active {
		return BuyerDSARExport{}, errNotFound
	}

	export := BuyerDSARExport{SchemaVersion: 1, GeneratedAt: time.Now().UTC()}
	queries := []struct {
		dst   *json.RawMessage
		query string
	}{
		{&export.Buyer, `SELECT to_jsonb(x) FROM (
			SELECT id,email,free_credit_usd,created_at FROM buyers WHERE id=$1) x`},
		{&export.Jobs, `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.created_at,x.id),'[]'::jsonb)
			FROM (SELECT id,created_at,status,job_type,model_ref,input_ref,output_ref,tier,
			verification_policy,estimated_usd,actual_usd,task_count,tasks_done,submit_idempotency_key,
			charge_status,stripe_pi,billed_usd,terminal_at
			FROM jobs WHERE buyer_id=$1) x`},
		{&export.Quotes, `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.created_at,x.id),'[]'::jsonb)
			FROM (SELECT id,created_at,job_type,model_ref,tier,records,input_bytes,estimated_tokens,
			malformed_records,split_size,task_count,eligible_now,cost_expected_usd,cost_min_usd,
			cost_max_usd,eta_p50_secs,eta_p90_secs,oom_risk,confidence,quote_json,
			economic_schedule_version,economic_plan,economic_executable
			FROM quotes WHERE buyer_id=$1) x`},
		{&export.Tasks, `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.created_at,x.id),'[]'::jsonb)
			FROM (SELECT t.id,t.job_id,t.created_at,t.started_at,t.completed_at,t.status,t.input_ref,
			t.result_ref,t.result_key,t.result_sha256,t.retry_count,t.chunk_index,t.execution_worker_id,
			t.execution_supplier_id,t.execution_hw_class,t.execution_engine,t.execution_build_hash,
			t.verification_outcome,t.verified_at
			FROM tasks t JOIN jobs j ON j.id=t.job_id WHERE j.buyer_id=$1) x`},
		{&export.Artifacts, `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.job_id,x.kind,x.ref),'[]'::jsonb)
			FROM (
			 SELECT id AS job_id,'job_input'::text AS kind,input_ref AS ref FROM jobs WHERE buyer_id=$1
			 UNION ALL SELECT id,'job_output',output_ref FROM jobs WHERE buyer_id=$1 AND output_ref IS NOT NULL
			 UNION ALL SELECT t.job_id,'task_input',t.input_ref FROM tasks t JOIN jobs j ON j.id=t.job_id
			   WHERE j.buyer_id=$1 AND t.input_ref IS NOT NULL
			 UNION ALL SELECT t.job_id,'task_result',COALESCE(t.result_ref,t.result_key) FROM tasks t
			   JOIN jobs j ON j.id=t.job_id WHERE j.buyer_id=$1 AND COALESCE(t.result_ref,t.result_key) IS NOT NULL
			) x`},
		{&export.Payments, `SELECT jsonb_build_object(
			'billing_customer',COALESCE((SELECT to_jsonb(x) FROM (
			 SELECT stripe_customer_id,default_payment_method,created_at,updated_at
			 FROM billing_customers WHERE buyer_id=$1) x),'null'::jsonb),
			'charge_batches',COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY x.created_at) FROM (
			 SELECT id,amount_usd,status,stripe_pi,created_at,charged_at,attempts
			 FROM charge_batches WHERE buyer_id=$1) x),'[]'::jsonb),
			'charge_operations',COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY x.created_at) FROM (
			 SELECT operation_key,source_kind,job_id,charge_batch_id,amount_cents,currency,status,
			 payment_intent,charge_id,last_error,created_at,updated_at
			 FROM buyer_charge_operations WHERE buyer_id=$1) x),'[]'::jsonb),
			'cash_collections',COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY x.recorded_at) FROM (
			 SELECT payment_intent,charge_id,source_kind,job_id,charge_batch_id,requested_cents,
			 received_cents,currency,recorded_at FROM buyer_cash_collections WHERE buyer_id=$1) x),'[]'::jsonb),
			'ledger',COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY x.created_at,x.id) FROM (
			 SELECT id,created_at,kind,task_id,amount_usd,payout_status,payout_ref
			 FROM ledger_entries WHERE buyer_id=$1) x),'[]'::jsonb))`},
		{&export.Webhooks, `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.created_at,x.id),'[]'::jsonb)
			FROM (SELECT id,job_id,url,created_at,delivered_at,attempts,next_attempt_at,
			dead_lettered_at,last_attempt_at,last_error FROM webhooks WHERE buyer_id=$1) x`},
		{&export.Disputes, `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.created_at,x.id),'[]'::jsonb)
			FROM (SELECT id,job_id,reason,status,created_at,resolved_at,filing_deadline
			FROM disputes WHERE buyer_id=$1) x`},
		{&export.AuthMetadata, `SELECT jsonb_build_object(
			'api_keys',COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY x.created_at) FROM (
			 SELECT id,name,masked,created_at,revoked FROM api_keys WHERE buyer_id=$1) x),'[]'::jsonb),
			'sessions',COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY x.created_at) FROM (
			 SELECT created_at,expires_at,revoked FROM sessions WHERE buyer_id=$1) x),'[]'::jsonb))`},
		{&export.Provenance, `SELECT jsonb_build_object(
			'job_events',COALESCE((SELECT jsonb_agg(to_jsonb(e) ORDER BY e.created_at,e.id)
			 FROM job_events e JOIN jobs j ON j.id=e.job_id WHERE j.buyer_id=$1),'[]'::jsonb),
			'execution_attempts',COALESCE((SELECT jsonb_agg(to_jsonb(h) ORDER BY h.started_at,h.attempt)
			 FROM task_execution_history h JOIN tasks t ON t.id=h.task_id
			 JOIN jobs j ON j.id=t.job_id WHERE j.buyer_id=$1),'[]'::jsonb),
			'verdicts',COALESCE((SELECT jsonb_agg(to_jsonb(v) ORDER BY v.created_at,v.attempt)
			 FROM task_verdicts v JOIN jobs j ON j.id=v.job_id WHERE j.buyer_id=$1),'[]'::jsonb),
			'ledger_entry_ids',COALESCE((SELECT jsonb_agg(id ORDER BY created_at,id)
			 FROM ledger_entries WHERE buyer_id=$1),'[]'::jsonb))`},
	}
	for _, item := range queries {
		raw, err := queryJSON(ctx, tx, item.query, buyerID)
		if err != nil {
			return BuyerDSARExport{}, err
		}
		*item.dst = raw
	}
	if err := tx.Commit(ctx); err != nil {
		return BuyerDSARExport{}, err
	}
	return export, nil
}

type BuyerDeletionResult struct {
	BuyerID           uuid.UUID `json:"buyer_id"`
	TombstoneID       uuid.UUID `json:"tombstone_id"`
	DeletedAt         time.Time `json:"deleted_at"`
	ArtifactRefs      []string  `json:"artifact_refs_to_delete,omitempty"`
	FinancialRetained bool      `json:"financial_records_retained"`
}

func artifactRefDigest(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:])
}

func collectBuyerArtifactRefs(ctx context.Context, tx pgx.Tx, buyerID uuid.UUID) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT ref FROM (
		 SELECT input_ref AS ref FROM jobs WHERE buyer_id=$1
		 UNION ALL SELECT output_ref FROM jobs WHERE buyer_id=$1
		 UNION ALL SELECT t.input_ref FROM tasks t JOIN jobs j ON j.id=t.job_id WHERE j.buyer_id=$1
		 UNION ALL SELECT t.result_ref FROM tasks t JOIN jobs j ON j.id=t.job_id WHERE j.buyer_id=$1
		 UNION ALL SELECT t.result_key FROM tasks t JOIN jobs j ON j.id=t.job_id WHERE j.buyer_id=$1
		) refs WHERE ref IS NOT NULL AND btrim(ref) <> ''`, buyerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]struct{}{}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		seen[ref] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs, nil
}

func enforceBuyerTombstone(ctx context.Context, tx pgx.Tx, buyerID, tombstoneID uuid.UUID, emailSHA256 string) error {
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM webhooks WHERE buyer_id=$1`, []any{buyerID}},
		{`DELETE FROM sessions WHERE buyer_id=$1`, []any{buyerID}},
		{`DELETE FROM api_keys WHERE buyer_id=$1`, []any{buyerID}},
		{`DELETE FROM billing_customers WHERE buyer_id=$1`, []any{buyerID}},
		{`DELETE FROM quotes WHERE buyer_id=$1`, []any{buyerID}},
		{`DELETE FROM worker_enrollment_codes WHERE buyer_id=$1`, []any{buyerID}},
		{`DELETE FROM alpha_requests WHERE encode(digest(lower(email),'sha256'),'hex')=$1`, []any{emailSHA256}},
		{`UPDATE worker_tokens SET revoked=true,revoked_at=COALESCE(revoked_at,now()),
		   revocation_reason=COALESCE(revocation_reason,'buyer account tombstoned')
		   WHERE supplier_id IN (SELECT id FROM suppliers WHERE owner_buyer_id=$1)`, []any{buyerID}},
		{`UPDATE suppliers SET status='suspended',
		   email='deleted+'||id::text||'@invalid.example' WHERE owner_buyer_id=$1`, []any{buyerID}},
		{`UPDATE disputes SET reason='Deleted buyer dispute retained for financial record'
		   WHERE buyer_id=$1`, []any{buyerID}},
		{`UPDATE job_events SET buyer_text='Account data deleted',detail='{"redacted":true}'::jsonb
		   WHERE job_id IN (SELECT id FROM jobs WHERE buyer_id=$1)`, []any{buyerID}},
		{`UPDATE task_failures SET message='Account data deleted'
		   WHERE job_id IN (SELECT id FROM jobs WHERE buyer_id=$1)`, []any{buyerID}},
		{`UPDATE tasks SET input_ref=NULL,result_ref=NULL,result_key=NULL
		   WHERE job_id IN (SELECT id FROM jobs WHERE buyer_id=$1)`, []any{buyerID}},
		{`UPDATE jobs SET input_ref='deleted/'||id::text||'/input',output_ref=NULL,
		   verification_policy=NULL WHERE buyer_id=$1`, []any{buyerID}},
		{`UPDATE buyers SET email='deleted+'||id::text||'@invalid.example',password_hash=NULL,
		   free_credit_usd=0,deleted_at=COALESCE(deleted_at,now()),deletion_tombstone_id=$2
		   WHERE id=$1`, []any{buyerID, tombstoneID}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
			return err
		}
	}
	return nil
}

func deletionResult(ctx context.Context, tx pgx.Tx, buyerID uuid.UUID, refs []string) (BuyerDeletionResult, error) {
	var result BuyerDeletionResult
	result.BuyerID = buyerID
	result.ArtifactRefs = refs
	result.FinancialRetained = true
	if err := tx.QueryRow(ctx, `
		SELECT deletion_tombstone_id,deleted_at FROM buyers WHERE id=$1`, buyerID).Scan(
		&result.TombstoneID, &result.DeletedAt); err != nil {
		return BuyerDeletionResult{}, err
	}
	return result, nil
}

func (s *Store) TombstoneBuyer(
	ctx context.Context,
	actor AdminActor,
	buyerID uuid.UUID,
	reason, correlationRef string,
) (BuyerDeletionResult, error) {
	intent, err := prepareAdminMutation(actor, adminMutationIntent{
		Kind: adminActionBuyerTombstoned, TargetKind: adminTargetBuyer,
		TargetID: buyerID, Reason: reason, CorrelationRef: correlationRef,
	})
	if err != nil {
		return BuyerDeletionResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BuyerDeletionResult{}, err
	}
	defer tx.Rollback(ctx)
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return BuyerDeletionResult{}, err
	}
	if replay, err := acquireAdminMutationReplay(ctx, tx, actor, intent); err != nil {
		return BuyerDeletionResult{}, err
	} else if replay.Found {
		result, err := deletionResult(ctx, tx, buyerID, nil)
		if err != nil {
			return BuyerDeletionResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return BuyerDeletionResult{}, err
		}
		return result, nil
	}

	var email string
	var alreadyDeleted bool
	if err := tx.QueryRow(ctx, `SELECT email,deleted_at IS NOT NULL FROM buyers WHERE id=$1 FOR UPDATE`,
		buyerID).Scan(&email, &alreadyDeleted); errors.Is(err, pgx.ErrNoRows) {
		return BuyerDeletionResult{}, errNotFound
	} else if err != nil {
		return BuyerDeletionResult{}, err
	}
	if alreadyDeleted {
		return BuyerDeletionResult{}, fmt.Errorf("%w: buyer is already tombstoned", errAdminMutationInvalid)
	}
	emailSHA256 := artifactRefDigest(strings.ToLower(strings.TrimSpace(email)))
	refs, err := collectBuyerArtifactRefs(ctx, tx, buyerID)
	if err != nil {
		return BuyerDeletionResult{}, err
	}
	refHashes := make([]string, len(refs))
	for index, ref := range refs {
		refHashes[index] = artifactRefDigest(ref)
	}
	refHashesJSON, err := json.Marshal(refHashes)
	if err != nil {
		return BuyerDeletionResult{}, err
	}
	tombstoneID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_identity_tombstones
		  (id,buyer_id,email_sha256,reason,correlation_ref,deleted_by,deleted_by_label,artifact_ref_sha256s)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		tombstoneID, buyerID, emailSHA256, intent.Reason, intent.CorrelationRef,
		actor.PrincipalID, strings.TrimSpace(actor.Label), json.RawMessage(refHashesJSON)); err != nil {
		return BuyerDeletionResult{}, err
	}
	if err := insertAdminMutationAction(ctx, tx, actor, intent, nil, nil, nil,
		map[string]any{"active": true, "email_sha256": emailSHA256, "artifact_count": len(refs)},
		map[string]any{"active": false, "tombstone_id": tombstoneID,
			"authentication_revoked": true, "artifact_refs_expired": true,
			"financial_records_retained": true}); err != nil {
		return BuyerDeletionResult{}, err
	}
	if err := enforceBuyerTombstone(ctx, tx, buyerID, tombstoneID, emailSHA256); err != nil {
		return BuyerDeletionResult{}, err
	}
	result, err := deletionResult(ctx, tx, buyerID, refs)
	if err != nil {
		return BuyerDeletionResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BuyerDeletionResult{}, err
	}
	return result, nil
}

// ApplyBuyerTombstones is run after restoring a database backup. Tombstones
// must be imported from the separately retained deletion journal first.
func (s *Store) ApplyBuyerTombstones(ctx context.Context) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id,buyer_id,email_sha256 FROM buyer_identity_tombstones ORDER BY created_at,id`)
	if err != nil {
		return 0, err
	}
	type tombstone struct {
		id, buyerID uuid.UUID
		emailSHA    string
	}
	var tombstones []tombstone
	for rows.Next() {
		var item tombstone
		if err := rows.Scan(&item.id, &item.buyerID, &item.emailSHA); err != nil {
			rows.Close()
			return 0, err
		}
		tombstones = append(tombstones, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, item := range tombstones {
		if err := enforceBuyerTombstone(ctx, tx, item.buyerID, item.id, item.emailSHA); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int64(len(tombstones)), nil
}
