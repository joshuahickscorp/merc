package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	workerEnrollmentProtocolVersion   = 2
	workerEnrollmentAudience          = "cx-macos-agent-v2"
	workerEnrollmentCodeTTL           = 10 * time.Minute
	workerEnrollmentMaxFailedAttempts = 5
	workerEnrollmentLabelMaxLen       = 120
	workerEnrollmentCodePrefix        = "cxe_"
	workerEnrollmentKeyAlgorithm      = "p256"
	workerEnrollmentRequestPrefix     = "cxer2_"
	workerEnrollmentBundlePrefix      = "cxeb2_"
	workerEnrollmentRequestMaxLen     = 8 << 10
	publicControlOriginEnv            = "CX_PUBLIC_CONTROL_ORIGIN"
)

var (
	errWorkerEnrollmentInvalid      = errors.New("invalid worker enrollment request")
	errWorkerEnrollmentDeviceActive = errors.New("device already has an active credential")
	errWorkerEnrollmentRejected     = errors.New("worker enrollment exchange rejected")
)

type EnrollmentCodeIssueInput struct {
	Audience               string     `json:"audience"`
	DeviceKeyAlgorithm     string     `json:"device_key_algorithm"`
	DevicePublicKey        string     `json:"device_public_key"`
	Label                  string     `json:"label,omitempty"`
	RotateFromCredentialID *uuid.UUID `json:"rotate_from_credential_id,omitempty"`
}

type EnrollmentCodeIssueResult struct {
	Version           int       `json:"v"`
	EnrollmentCodeID  uuid.UUID `json:"enrollment_code_id"`
	EnrollmentCode    string    `json:"enrollment_code"`
	ControlOrigin     string    `json:"control_origin"`
	RequestID         string    `json:"request_id"`
	Audience          string    `json:"audience"`
	AccountID         uuid.UUID `json:"account_id"`
	SupplierID        uuid.UUID `json:"supplier_id"`
	DeviceFingerprint string    `json:"device_fingerprint"`
	ExpiresAt         time.Time `json:"expires_at"`
	Rotation          bool      `json:"rotation"`
}

type EnrollmentDeviceRequest struct {
	Version            int    `json:"v"`
	ControlOrigin      string `json:"control_origin"`
	Audience           string `json:"audience"`
	RequestID          string `json:"request_id"`
	DeviceKeyAlgorithm string `json:"device_key_algorithm"`
	DevicePublicKey    string `json:"device_public_key"`
}

type EnrollmentApprovalInput struct {
	DeviceRequest string `json:"device_request"`
	Label         string `json:"label,omitempty"`
}

type EnrollmentApprovalResult struct {
	EnrollmentBundle string    `json:"enrollment_bundle"`
	EnrollmentCodeID uuid.UUID `json:"enrollment_code_id"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type enrollmentApprovalBundlePayload struct {
	Version           int       `json:"v"`
	ControlOrigin     string    `json:"control_origin"`
	AccountID         uuid.UUID `json:"account_id"`
	Audience          string    `json:"audience"`
	EnrollmentCode    string    `json:"enrollment_code"`
	RequestID         string    `json:"request_id"`
	DeviceFingerprint string    `json:"device_fingerprint"`
}

type EnrollmentExchangeInput struct {
	Version            int       `json:"v"`
	EnrollmentCode     string    `json:"enrollment_code"`
	ControlOrigin      string    `json:"control_origin"`
	RequestID          string    `json:"request_id"`
	Audience           string    `json:"audience"`
	AccountID          uuid.UUID `json:"account_id"`
	DeviceKeyAlgorithm string    `json:"device_key_algorithm"`
	DevicePublicKey    string    `json:"device_public_key"`
	Proof              string    `json:"proof"`
}

type EnrollmentExchangeResult struct {
	CredentialID      uuid.UUID `json:"credential_id"`
	WorkerID          uuid.UUID `json:"worker_id"`
	SupplierID        uuid.UUID `json:"supplier_id"`
	WorkerToken       string    `json:"worker_token"`
	DeviceFingerprint string    `json:"device_fingerprint"`
	CredentialVersion int       `json:"credential_version"`
	Rotated           bool      `json:"rotated"`
}

type enrollmentRequestBinding struct {
	Version       int
	ControlOrigin string
	RequestID     string
}

type WorkerCredentialView struct {
	CredentialID          uuid.UUID  `json:"credential_id"`
	WorkerID              uuid.UUID  `json:"worker_id"`
	Label                 string     `json:"label,omitempty"`
	EnrollmentDeviceBound bool       `json:"enrollment_device_bound"`
	DeviceKeyAlgorithm    string     `json:"device_key_algorithm,omitempty"`
	DeviceFingerprint     string     `json:"device_fingerprint,omitempty"`
	CredentialVersion     int        `json:"credential_version"`
	RotatedFrom           *uuid.UUID `json:"rotated_from_credential_id,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	Revoked               bool       `json:"revoked"`
	RevokedAt             *time.Time `json:"revoked_at,omitempty"`
	RevocationReason      string     `json:"revocation_reason,omitempty"`
}

type WorkerCredentialAuditView struct {
	ID               uuid.UUID       `json:"id"`
	EventType        string          `json:"event_type"`
	WorkerID         *uuid.UUID      `json:"worker_id,omitempty"`
	EnrollmentCodeID *uuid.UUID      `json:"enrollment_code_id,omitempty"`
	CredentialID     *uuid.UUID      `json:"credential_id,omitempty"`
	Reason           string          `json:"reason,omitempty"`
	Detail           json.RawMessage `json:"detail"`
	CreatedAt        time.Time       `json:"created_at"`
}

func parseP256EnrollmentPublicKey(encoded string) ([]byte, *ecdsa.PublicKey, string, error) {
	if strings.TrimSpace(encoded) != encoded || encoded == "" {
		return nil, nil, "", errors.New("device_public_key must be unpadded base64url")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded || len(raw) != 65 || raw[0] != 4 {
		return nil, nil, "", errors.New("device_public_key must be a P-256 uncompressed point")
	}
	publicKey, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
	if err != nil {
		return nil, nil, "", errors.New("device_public_key is not on P-256")
	}
	canonical, err := publicKey.Bytes()
	if err != nil {
		return nil, nil, "", errors.New("device_public_key is not on P-256")
	}
	sum := sha256.Sum256(append([]byte(workerEnrollmentKeyAlgorithm+"\x00"), canonical...))
	return canonical, publicKey,
		workerEnrollmentKeyAlgorithm + ":sha256:" + hex.EncodeToString(sum[:]), nil
}

func enrollmentRequestID(publicKey []byte) string {
	material := append([]byte("cx-enrollment-request-v1\x00"), publicKey...)
	digest := sha256.Sum256(material)
	return base64.RawURLEncoding.EncodeToString(digest[:16])
}

func decodeExactEnrollmentObject(data []byte, dst any, expectedKeys ...string) error {
	expected := make(map[string]struct{}, len(expectedKeys))
	for _, key := range expectedKeys {
		expected[key] = struct{}{}
	}
	seen := make(map[string]struct{}, len(expectedKeys))
	dec := json.NewDecoder(bytes.NewReader(data))
	first, err := dec.Token()
	if err != nil || first != json.Delim('{') {
		return errors.New("enrollment payload must be one JSON object")
	}
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("enrollment payload has a non-string key")
		}
		if _, ok := expected[key]; !ok {
			return fmt.Errorf("unexpected enrollment key %q", key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("duplicate enrollment key %q", key)
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
	}
	last, err := dec.Token()
	if err != nil || last != json.Delim('}') {
		return errors.New("enrollment payload is not a complete JSON object")
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("enrollment payload must contain one JSON object")
		}
		return err
	}
	if len(seen) != len(expected) {
		return errors.New("enrollment payload is missing required keys")
	}
	return json.Unmarshal(data, dst)
}

func canonicalEnrollmentControlOrigin(raw string, allowInsecureLoopback bool) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", errors.New("control_origin must not contain outer whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Scheme == "" || u.Host == "" || u.User != nil {
		return "", errors.New("control_origin must be an absolute origin")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("control_origin must not contain a path")
	}
	if u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return "", errors.New("control_origin must not contain a query or fragment")
	}
	scheme := strings.ToLower(u.Scheme)
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" || strings.Contains(hostname, "%") {
		return "", errors.New("control_origin has an invalid host")
	}
	port := u.Port()
	loopback := hostname == "localhost"
	if ip := net.ParseIP(hostname); ip != nil && ip.IsLoopback() {
		loopback = true
	}
	if scheme != "https" && !(allowInsecureLoopback && scheme == "http" && loopback) {
		return "", errors.New("control_origin must use HTTPS")
	}
	canonicalHost := hostname
	if port != "" {
		canonicalHost = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		canonicalHost = "[" + hostname + "]"
	}
	return scheme + "://" + canonicalHost, nil
}

func decodeEnrollmentDeviceRequest(encoded, expectedOrigin string) (EnrollmentDeviceRequest, error) {
	var zero EnrollmentDeviceRequest
	value := strings.TrimSpace(encoded)
	if value == "" || len(value) > workerEnrollmentRequestMaxLen || !strings.HasPrefix(value, workerEnrollmentRequestPrefix) {
		return zero, errWorkerEnrollmentInvalid
	}
	payloadText := strings.TrimPrefix(value, workerEnrollmentRequestPrefix)
	payload, err := base64.RawURLEncoding.DecodeString(payloadText)
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != payloadText {
		return zero, errWorkerEnrollmentInvalid
	}
	var request EnrollmentDeviceRequest
	if err := decodeExactEnrollmentObject(payload, &request,
		"v", "control_origin", "audience", "request_id", "device_key_algorithm", "device_public_key"); err != nil {
		return zero, fmt.Errorf("%w: %v", errWorkerEnrollmentInvalid, err)
	}
	if request.Version != workerEnrollmentProtocolVersion || request.Audience != workerEnrollmentAudience ||
		request.DeviceKeyAlgorithm != workerEnrollmentKeyAlgorithm {
		return zero, errWorkerEnrollmentInvalid
	}
	publicKey, _, _, err := parseP256EnrollmentPublicKey(request.DevicePublicKey)
	if err != nil || request.RequestID != enrollmentRequestID(publicKey) {
		return zero, errWorkerEnrollmentInvalid
	}
	canonicalExpected, err := canonicalEnrollmentControlOrigin(expectedOrigin, true)
	if err != nil {
		return zero, fmt.Errorf("%w: invalid expected origin", errWorkerEnrollmentInvalid)
	}
	allowInsecureLoopback := strings.HasPrefix(canonicalExpected, "http://")
	canonicalRequest, err := canonicalEnrollmentControlOrigin(request.ControlOrigin, allowInsecureLoopback)
	if err != nil || request.ControlOrigin != canonicalRequest || canonicalRequest != canonicalExpected {
		return zero, errWorkerEnrollmentInvalid
	}
	return request, nil
}

func enrollmentControlOriginForRequest(r *http.Request) (string, error) {
	if configured := os.Getenv(publicControlOriginEnv); configured != "" {
		canonical, err := canonicalEnrollmentControlOrigin(configured, false)
		if err != nil || canonical != configured {
			return "", fmt.Errorf("%s must be one canonical HTTPS origin", publicControlOriginEnv)
		}
		return canonical, nil
	}
	peerHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peerHost = r.RemoteAddr
	}
	peerIP := net.ParseIP(strings.Trim(peerHost, "[]"))
	if peerIP == nil || !peerIP.IsLoopback() {
		return "", errors.New("public enrollment origin is not configured for a non-loopback request")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	origin, err := canonicalEnrollmentControlOrigin(scheme+"://"+r.Host, true)
	if err != nil {
		return "", errors.New("unconfigured enrollment approval is allowed only on loopback")
	}
	u, _ := url.Parse(origin)
	hostname := u.Hostname()
	hostLoopback := hostname == "localhost"
	if ip := net.ParseIP(hostname); ip != nil && ip.IsLoopback() {
		hostLoopback = true
	}
	if !hostLoopback {
		return "", errors.New("unconfigured enrollment approval host must be loopback")
	}
	return origin, nil
}

func encodeEnrollmentApprovalBundle(request EnrollmentDeviceRequest, issued EnrollmentCodeIssueResult) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"v":                  workerEnrollmentProtocolVersion,
		"control_origin":     request.ControlOrigin,
		"account_id":         issued.AccountID,
		"audience":           issued.Audience,
		"enrollment_code":    issued.EnrollmentCode,
		"request_id":         request.RequestID,
		"device_fingerprint": issued.DeviceFingerprint,
	})
	if err != nil {
		return "", err
	}
	return workerEnrollmentBundlePrefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

func validEnrollmentCode(raw string) bool {
	if !strings.HasPrefix(raw, workerEnrollmentCodePrefix) || strings.TrimSpace(raw) != raw {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, workerEnrollmentCodePrefix))
	return err == nil && len(b) == 32
}

func enrollmentExchangeTranscript(
	code, audience string,
	accountID uuid.UUID,
	controlOrigin, requestID string,
) []byte {
	return []byte("cx-worker-enrollment-exchange-v2\n" + audience + "\n" + accountID.String() +
		"\n" + controlOrigin + "\n" + requestID + "\n" + code)
}

func verifyEnrollmentProof(pub *ecdsa.PublicKey, proof string, transcript []byte) bool {
	raw, err := base64.RawURLEncoding.DecodeString(proof)
	if err != nil || len(raw) < 8 || len(raw) > 80 {
		return false
	}
	digest := sha256.Sum256(transcript)
	return ecdsa.VerifyASN1(pub, digest[:], raw)
}

func auditDetail(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

func insertWorkerCredentialAuditTx(
	ctx context.Context,
	tx pgx.Tx,
	eventType string,
	buyerID, supplierID uuid.UUID,
	workerID, codeID, credentialID *uuid.UUID,
	reason string,
	detail any,
) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO worker_credential_audit
		  (event_type,buyer_id,supplier_id,worker_id,enrollment_code_id,credential_id,reason,detail)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8)`,
		eventType, buyerID, supplierID, workerID, codeID, credentialID, reason, auditDetail(detail))
	return err
}

func lockEnrollmentFingerprintTx(ctx context.Context, tx pgx.Tx, fingerprint string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, fingerprint)
	return err
}

func nullEnrollmentRotationSource(source *uuid.UUID) uuid.UUID {
	if source == nil {
		return uuid.Nil
	}
	return *source
}

func revokePendingEnrollmentCodesTx(
	ctx context.Context,
	tx pgx.Tx,
	buyerID, supplierID, excludeID uuid.UUID,
	fingerprint string,
	rotationSource uuid.UUID,
	reason string,
) error {
	rows, err := tx.Query(ctx, `
		UPDATE worker_enrollment_codes
		   SET revoked_at=clock_timestamp()
		 WHERE buyer_id=$1 AND supplier_id=$2
		   AND consumed_at IS NULL AND revoked_at IS NULL
		   AND ($3::uuid = '00000000-0000-0000-0000-000000000000'::uuid OR id<>$3)
		   AND (($4<>'' AND device_fingerprint=$4)
		        OR ($5::uuid <> '00000000-0000-0000-0000-000000000000'::uuid
		            AND rotate_from_credential_id=$5))
		 RETURNING id`, buyerID, supplierID, excludeID, fingerprint, rotationSource)
	if err != nil {
		return err
	}
	var codeIDs []uuid.UUID
	for rows.Next() {
		var codeID uuid.UUID
		if err := rows.Scan(&codeID); err != nil {
			rows.Close()
			return err
		}
		codeIDs = append(codeIDs, codeID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, codeID := range codeIDs {
		var source *uuid.UUID
		if rotationSource != uuid.Nil {
			source = &rotationSource
		}
		if err := insertWorkerCredentialAuditTx(ctx, tx, "code_revoked", buyerID, supplierID,
			nil, &codeID, source, reason, map[string]any{"automatic": true}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CreateWorkerEnrollmentCode(
	ctx context.Context,
	buyerID uuid.UUID,
	in EnrollmentCodeIssueInput,
	binding enrollmentRequestBinding,
) (EnrollmentCodeIssueResult, error) {
	if in.Audience != workerEnrollmentAudience || in.DeviceKeyAlgorithm != workerEnrollmentKeyAlgorithm {
		return EnrollmentCodeIssueResult{}, fmt.Errorf("%w: unsupported audience or device key algorithm", errWorkerEnrollmentInvalid)
	}
	pubBytes, _, fingerprint, err := parseP256EnrollmentPublicKey(in.DevicePublicKey)
	if err != nil {
		return EnrollmentCodeIssueResult{}, fmt.Errorf("%w: %v", errWorkerEnrollmentInvalid, err)
	}
	allowInsecureLoopback := strings.HasPrefix(binding.ControlOrigin, "http://")
	canonicalOrigin, originErr := canonicalEnrollmentControlOrigin(binding.ControlOrigin, allowInsecureLoopback)
	wantRequestID := enrollmentRequestID(pubBytes)
	if binding.Version != workerEnrollmentProtocolVersion || originErr != nil ||
		canonicalOrigin != binding.ControlOrigin || binding.RequestID != wantRequestID {
		return EnrollmentCodeIssueResult{}, fmt.Errorf("%w: invalid trusted request binding", errWorkerEnrollmentInvalid)
	}
	label := strings.TrimSpace(in.Label)
	if len(label) > workerEnrollmentLabelMaxLen {
		return EnrollmentCodeIssueResult{}, fmt.Errorf("%w: label exceeds %d bytes", errWorkerEnrollmentInvalid, workerEnrollmentLabelMaxLen)
	}
	supplierID, err := s.EnsureSupplierForBuyer(ctx, buyerID)
	if err != nil {
		return EnrollmentCodeIssueResult{}, err
	}
	raw := newSecret(workerEnrollmentCodePrefix)
	if !validEnrollmentCode(raw) {
		return EnrollmentCodeIssueResult{}, errors.New("enrollment code entropy failure")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return EnrollmentCodeIssueResult{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockEnrollmentFingerprintTx(ctx, tx, fingerprint); err != nil {
		return EnrollmentCodeIssueResult{}, err
	}

	if in.RotateFromCredentialID != nil {
		var sourceID uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT wt.credential_id
			  FROM worker_tokens wt JOIN suppliers sp ON sp.id=wt.supplier_id
			 WHERE wt.credential_id=$1 AND sp.owner_buyer_id=$2 AND wt.revoked=false
			 FOR SHARE OF wt`, *in.RotateFromCredentialID, buyerID).Scan(&sourceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return EnrollmentCodeIssueResult{}, errNotFound
		}
		if err != nil {
			return EnrollmentCodeIssueResult{}, err
		}
	}

	var activeCredential uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT credential_id FROM worker_tokens
		 WHERE device_fingerprint=$1 AND revoked=false LIMIT 1`, fingerprint).Scan(&activeCredential)
	if err == nil && (in.RotateFromCredentialID == nil || activeCredential != *in.RotateFromCredentialID) {
		return EnrollmentCodeIssueResult{}, errWorkerEnrollmentDeviceActive
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return EnrollmentCodeIssueResult{}, err
	}
	if err := revokePendingEnrollmentCodesTx(ctx, tx, buyerID, supplierID, uuid.Nil,
		fingerprint, nullEnrollmentRotationSource(in.RotateFromCredentialID), "superseded_by_new_code"); err != nil {
		return EnrollmentCodeIssueResult{}, err
	}

	var result EnrollmentCodeIssueResult
	result.Version = binding.Version
	result.EnrollmentCode = raw
	result.ControlOrigin = binding.ControlOrigin
	result.RequestID = binding.RequestID
	result.Audience = in.Audience
	result.AccountID = buyerID
	result.SupplierID = supplierID
	result.DeviceFingerprint = fingerprint
	result.Rotation = in.RotateFromCredentialID != nil
	err = tx.QueryRow(ctx, `
		INSERT INTO worker_enrollment_codes
		  (code_hash,buyer_id,supplier_id,protocol_version,control_origin,request_id,
		   audience,device_key_algorithm,device_public_key,device_fingerprint,label,
		   rotate_from_credential_id,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),$12,
		        clock_timestamp()+make_interval(secs=>$13))
		RETURNING id,expires_at`,
		hashKey(raw), buyerID, supplierID, binding.Version, binding.ControlOrigin, binding.RequestID,
		in.Audience, in.DeviceKeyAlgorithm, pubBytes, fingerprint, label,
		in.RotateFromCredentialID, workerEnrollmentCodeTTL.Seconds()).
		Scan(&result.EnrollmentCodeID, &result.ExpiresAt)
	if err != nil {
		return EnrollmentCodeIssueResult{}, err
	}
	if err := insertWorkerCredentialAuditTx(ctx, tx, "code_issued", buyerID, supplierID,
		nil, &result.EnrollmentCodeID, in.RotateFromCredentialID, "", map[string]any{
			"audience": in.Audience, "device_fingerprint": fingerprint, "rotation": result.Rotation,
			"protocol_version": binding.Version, "control_origin": binding.ControlOrigin,
			"request_id": binding.RequestID,
		}); err != nil {
		return EnrollmentCodeIssueResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return EnrollmentCodeIssueResult{}, err
	}
	return result, nil
}

type enrollmentCodeRow struct {
	id, buyerID, supplierID uuid.UUID
	protocolVersion         int
	controlOrigin           string
	requestID               string
	audience, fingerprint   string
	publicKey               []byte
	label                   string
	expired                 bool
	consumedAt, revokedAt   *time.Time
	failedAttempts          int
	rotateFrom              *uuid.UUID
}

func (s *Store) EnrollWorkerTx(ctx context.Context, in EnrollmentExchangeInput) (EnrollmentExchangeResult, error) {
	if !validEnrollmentCode(in.EnrollmentCode) {
		return EnrollmentExchangeResult{}, errWorkerEnrollmentRejected
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return EnrollmentExchangeResult{}, err
	}
	defer tx.Rollback(ctx)

	var knownFingerprint string
	err = tx.QueryRow(ctx, `
		SELECT device_fingerprint FROM worker_enrollment_codes WHERE code_hash=$1`,
		hashKey(in.EnrollmentCode)).Scan(&knownFingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnrollmentExchangeResult{}, errWorkerEnrollmentRejected
	}
	if err != nil {
		return EnrollmentExchangeResult{}, err
	}
	if err := lockEnrollmentFingerprintTx(ctx, tx, knownFingerprint); err != nil {
		return EnrollmentExchangeResult{}, err
	}

	var code enrollmentCodeRow
	err = tx.QueryRow(ctx, `
		SELECT c.id,c.buyer_id,c.supplier_id,c.protocol_version,COALESCE(c.control_origin,''),
		       COALESCE(c.request_id,''),c.audience,c.device_public_key,c.device_fingerprint,
		       COALESCE(c.label,''),c.expires_at <= clock_timestamp(),c.consumed_at,c.revoked_at,
		       c.failed_attempts,c.rotate_from_credential_id
		  FROM worker_enrollment_codes c
		  JOIN suppliers sp ON sp.id=c.supplier_id AND sp.owner_buyer_id=c.buyer_id
		 WHERE c.code_hash=$1
		 FOR UPDATE OF c`, hashKey(in.EnrollmentCode)).Scan(
		&code.id, &code.buyerID, &code.supplierID, &code.protocolVersion, &code.controlOrigin,
		&code.requestID, &code.audience, &code.publicKey, &code.fingerprint,
		&code.label, &code.expired, &code.consumedAt, &code.revokedAt, &code.failedAttempts, &code.rotateFrom)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnrollmentExchangeResult{}, errWorkerEnrollmentRejected
	}
	if err != nil {
		return EnrollmentExchangeResult{}, err
	}
	if err := tx.QueryRow(ctx, `
		SELECT expires_at <= clock_timestamp() FROM worker_enrollment_codes WHERE id=$1`,
		code.id).Scan(&code.expired); err != nil {
		return EnrollmentExchangeResult{}, err
	}

	reject := func(reason string, terminal bool) (EnrollmentExchangeResult, error) {
		if (code.consumedAt != nil || code.revokedAt != nil) && code.failedAttempts >= 1 {
			return EnrollmentExchangeResult{}, fmt.Errorf("%w: %s", errWorkerEnrollmentRejected, reason)
		}
		attempt := code.failedAttempts + 1
		if attempt > workerEnrollmentMaxFailedAttempts {
			attempt = workerEnrollmentMaxFailedAttempts
		}
		autoRevokeReason := ""
		if terminal && code.consumedAt == nil && code.revokedAt == nil {
			autoRevokeReason = reason
		} else if !terminal && code.consumedAt == nil && code.revokedAt == nil &&
			attempt >= workerEnrollmentMaxFailedAttempts {
			autoRevokeReason = "max_failed_attempts"
		}
		if _, err := tx.Exec(ctx, `
			UPDATE worker_enrollment_codes
			   SET failed_attempts=$2::int,last_attempt_at=clock_timestamp(),
			       revoked_at=CASE WHEN $3::boolean THEN COALESCE(revoked_at,clock_timestamp())
			                       ELSE revoked_at END
			 WHERE id=$1`, code.id, attempt, autoRevokeReason != ""); err != nil {
			return EnrollmentExchangeResult{}, err
		}
		if err := insertWorkerCredentialAuditTx(ctx, tx, "exchange_rejected", code.buyerID, code.supplierID,
			nil, &code.id, code.rotateFrom, reason, map[string]any{"attempt": attempt}); err != nil {
			return EnrollmentExchangeResult{}, err
		}
		if autoRevokeReason != "" {
			if err := insertWorkerCredentialAuditTx(ctx, tx, "code_revoked", code.buyerID, code.supplierID,
				nil, &code.id, code.rotateFrom, autoRevokeReason, map[string]any{"automatic": true}); err != nil {
				return EnrollmentExchangeResult{}, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return EnrollmentExchangeResult{}, err
		}
		return EnrollmentExchangeResult{}, fmt.Errorf("%w: %s", errWorkerEnrollmentRejected, reason)
	}

	switch {
	case code.consumedAt != nil:
		return reject("replay", true)
	case code.revokedAt != nil:
		return reject("revoked", true)
	case code.expired:
		return reject("expired", true)
	case in.Version != workerEnrollmentProtocolVersion || code.protocolVersion != workerEnrollmentProtocolVersion:
		return reject("wrong_protocol_version", true)
	case in.ControlOrigin != code.controlOrigin:
		return reject("wrong_control_origin", false)
	case in.RequestID != code.requestID:
		return reject("wrong_request_id", false)
	case in.Audience != workerEnrollmentAudience || in.Audience != code.audience:
		return reject("wrong_audience", false)
	case in.AccountID == uuid.Nil:
		return reject("invalid_account", false)
	case in.AccountID != code.buyerID:
		return reject("wrong_account", false)
	case in.DeviceKeyAlgorithm != workerEnrollmentKeyAlgorithm:
		return reject("wrong_device_key_algorithm", false)
	}
	requestedKey, requestedPub, requestedFingerprint, err := parseP256EnrollmentPublicKey(in.DevicePublicKey)
	if err != nil {
		return reject("invalid_device_key", false)
	}
	if requestedFingerprint != code.fingerprint || len(requestedKey) != len(code.publicKey) ||
		subtle.ConstantTimeCompare(requestedKey, code.publicKey) != 1 {
		return reject("wrong_device", false)
	}
	if in.RequestID != enrollmentRequestID(requestedKey) {
		return reject("wrong_request_id", false)
	}
	if !verifyEnrollmentProof(requestedPub, in.Proof,
		enrollmentExchangeTranscript(in.EnrollmentCode, code.audience, code.buyerID,
			code.controlOrigin, code.requestID)) {
		return reject("invalid_device_proof", false)
	}

	workerID := uuid.New()
	version := 1
	rotated := false
	if code.rotateFrom != nil {
		err := tx.QueryRow(ctx, `
			SELECT worker_id,credential_version
			  FROM worker_tokens
			 WHERE credential_id=$1 AND supplier_id=$2 AND revoked=false
			 FOR UPDATE`, *code.rotateFrom, code.supplierID).Scan(&workerID, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return reject("rotation_source_unavailable", true)
		}
		if err != nil {
			return EnrollmentExchangeResult{}, err
		}
		version++
		rotated = true
	}
	var activeDeviceCredential uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT credential_id FROM worker_tokens
		 WHERE device_fingerprint=$1 AND revoked=false
		   AND ($2::uuid IS NULL OR credential_id<>$2)
		 LIMIT 1`, code.fingerprint, code.rotateFrom).Scan(&activeDeviceCredential)
	if err == nil {
		return reject("device_already_enrolled", true)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return EnrollmentExchangeResult{}, err
	}

	if !rotated {
		if _, err := tx.Exec(ctx,
			`INSERT INTO workers (id,supplier_id,hw_class) VALUES ($1,$2,'cpu')`, workerID, code.supplierID); err != nil {
			return EnrollmentExchangeResult{}, err
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE worker_tokens
			   SET revoked=true,revoked_at=now(),revocation_reason='rotated'
			 WHERE credential_id=$1`, *code.rotateFrom); err != nil {
			return EnrollmentExchangeResult{}, err
		}
	}

	rawToken := newSecret("cxw_")
	if rawToken == "" {
		return EnrollmentExchangeResult{}, errors.New("worker token entropy failure")
	}
	credentialID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO worker_tokens
		  (token_hash,worker_id,supplier_id,revoked,credential_id,device_key_algorithm,
		   device_public_key,device_fingerprint,credential_version,rotated_from_credential_id,label)
		VALUES ($1,$2,$3,false,$4,$5,$6,$7,$8,$9,NULLIF($10,''))`,
		hashKey(rawToken), workerID, code.supplierID, credentialID, workerEnrollmentKeyAlgorithm,
		requestedKey, code.fingerprint, version, code.rotateFrom, code.label); err != nil {
		return EnrollmentExchangeResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE suppliers SET status='active' WHERE id=$1 AND status='pending'`, code.supplierID); err != nil {
		return EnrollmentExchangeResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE worker_enrollment_codes
		   SET consumed_at=clock_timestamp(),consumed_credential_id=$2,last_attempt_at=clock_timestamp()
		 WHERE id=$1 AND consumed_at IS NULL`, code.id, credentialID); err != nil {
		return EnrollmentExchangeResult{}, err
	}
	rotationSource := uuid.Nil
	if code.rotateFrom != nil {
		rotationSource = *code.rotateFrom
	}
	if err := revokePendingEnrollmentCodesTx(ctx, tx, code.buyerID, code.supplierID,
		code.id, code.fingerprint, rotationSource, "superseded_by_exchange"); err != nil {
		return EnrollmentExchangeResult{}, err
	}
	if err := insertWorkerCredentialAuditTx(ctx, tx, "exchange_succeeded", code.buyerID, code.supplierID,
		&workerID, &code.id, &credentialID, "", map[string]any{
			"device_fingerprint": code.fingerprint, "credential_version": version, "rotated": rotated,
		}); err != nil {
		return EnrollmentExchangeResult{}, err
	}
	if rotated {
		if err := insertWorkerCredentialAuditTx(ctx, tx, "credential_rotated", code.buyerID, code.supplierID,
			&workerID, &code.id, &credentialID, code.rotateFrom.String(), map[string]any{
				"replaced_credential_id": code.rotateFrom, "device_fingerprint": code.fingerprint,
			}); err != nil {
			return EnrollmentExchangeResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return EnrollmentExchangeResult{}, err
	}
	return EnrollmentExchangeResult{
		CredentialID: credentialID, WorkerID: workerID, SupplierID: code.supplierID,
		WorkerToken: rawToken, DeviceFingerprint: code.fingerprint,
		CredentialVersion: version, Rotated: rotated,
	}, nil
}

func (s *Store) ListWorkerCredentialsForBuyer(ctx context.Context, buyerID uuid.UUID) ([]WorkerCredentialView, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT wt.credential_id,wt.worker_id,COALESCE(wt.label,''),
		       wt.device_fingerprint IS NOT NULL,COALESCE(wt.device_key_algorithm,''),
		       COALESCE(wt.device_fingerprint,''),wt.credential_version,
		       wt.rotated_from_credential_id,wt.created_at,wt.revoked,wt.revoked_at,
		       COALESCE(wt.revocation_reason,'')
		  FROM worker_tokens wt JOIN suppliers sp ON sp.id=wt.supplier_id
		 WHERE sp.owner_buyer_id=$1
		 ORDER BY wt.created_at DESC,wt.credential_id`, buyerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkerCredentialView{}
	for rows.Next() {
		var v WorkerCredentialView
		if err := rows.Scan(&v.CredentialID, &v.WorkerID, &v.Label, &v.EnrollmentDeviceBound,
			&v.DeviceKeyAlgorithm, &v.DeviceFingerprint, &v.CredentialVersion,
			&v.RotatedFrom, &v.CreatedAt, &v.Revoked, &v.RevokedAt, &v.RevocationReason); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) RevokeWorkerCredentialForBuyer(ctx context.Context, buyerID, credentialID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var supplierID uuid.UUID
	var targetFingerprint string
	err = tx.QueryRow(ctx, `
		SELECT wt.supplier_id,COALESCE(wt.device_fingerprint,'')
		  FROM worker_tokens wt JOIN suppliers sp ON sp.id=wt.supplier_id
		 WHERE wt.credential_id=$1 AND sp.owner_buyer_id=$2`, credentialID, buyerID).
		Scan(&supplierID, &targetFingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if targetFingerprint != "" {
		if err := lockEnrollmentFingerprintTx(ctx, tx, targetFingerprint); err != nil {
			return err
		}
	}
	if err := revokePendingEnrollmentCodesTx(ctx, tx, buyerID, supplierID, uuid.Nil,
		targetFingerprint, credentialID, "credential_revoked"); err != nil {
		return err
	}

	rows, err := tx.Query(ctx, `
		WITH RECURSIVE chain AS (
		  SELECT credential_id,device_fingerprint FROM worker_tokens
		   WHERE credential_id=$1 AND supplier_id=$2
		  UNION
		  SELECT child.credential_id,child.device_fingerprint
		    FROM worker_tokens child JOIN chain parent
		      ON child.rotated_from_credential_id=parent.credential_id
		   WHERE child.supplier_id=$2
		)
		SELECT credential_id,COALESCE(device_fingerprint,'') FROM chain`, credentialID, supplierID)
	if err != nil {
		return err
	}
	type chainCredential struct {
		id          uuid.UUID
		fingerprint string
	}
	var chain []chainCredential
	for rows.Next() {
		var item chainCredential
		if err := rows.Scan(&item.id, &item.fingerprint); err != nil {
			rows.Close()
			return err
		}
		chain = append(chain, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	fingerprints := make([]string, 0, len(chain))
	seen := map[string]bool{targetFingerprint: targetFingerprint != ""}
	for _, item := range chain {
		if item.fingerprint != "" && !seen[item.fingerprint] {
			seen[item.fingerprint] = true
			fingerprints = append(fingerprints, item.fingerprint)
		}
	}
	sort.Strings(fingerprints)
	for _, fingerprint := range fingerprints {
		if err := lockEnrollmentFingerprintTx(ctx, tx, fingerprint); err != nil {
			return err
		}
	}
	for _, item := range chain {
		if item.id == credentialID {
			continue
		}
		if err := revokePendingEnrollmentCodesTx(ctx, tx, buyerID, supplierID, uuid.Nil,
			item.fingerprint, item.id, "credential_revoked"); err != nil {
			return err
		}
	}

	revokedRows, err := tx.Query(ctx, `
		WITH RECURSIVE chain AS (
		  SELECT credential_id FROM worker_tokens WHERE credential_id=$1 AND supplier_id=$2
		  UNION
		  SELECT child.credential_id FROM worker_tokens child JOIN chain parent
		    ON child.rotated_from_credential_id=parent.credential_id
		   WHERE child.supplier_id=$2
		)
		UPDATE worker_tokens wt
		   SET revoked=true,revoked_at=clock_timestamp(),revocation_reason='account_revoked'
		  FROM chain
		 WHERE wt.credential_id=chain.credential_id AND wt.revoked=false
		 RETURNING wt.worker_id,wt.credential_id`, credentialID, supplierID)
	if err != nil {
		return err
	}
	type revokedCredential struct {
		workerID     uuid.UUID
		credentialID uuid.UUID
	}
	var revokedCredentials []revokedCredential
	for revokedRows.Next() {
		var item revokedCredential
		if err := revokedRows.Scan(&item.workerID, &item.credentialID); err != nil {
			revokedRows.Close()
			return err
		}
		revokedCredentials = append(revokedCredentials, item)
	}
	if err := revokedRows.Err(); err != nil {
		revokedRows.Close()
		return err
	}
	revokedRows.Close()
	for _, item := range revokedCredentials {
		if err := insertWorkerCredentialAuditTx(ctx, tx, "credential_revoked", buyerID, supplierID,
			&item.workerID, nil, &item.credentialID, "account_revoked", map[string]any{
				"requested_credential_id": credentialID,
			}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) RevokeWorkerEnrollmentCodeForBuyer(ctx context.Context, buyerID, codeID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var supplierID uuid.UUID
	var revokedAt, consumedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT supplier_id,revoked_at,consumed_at FROM worker_enrollment_codes
		 WHERE id=$1 AND buyer_id=$2 FOR UPDATE`, codeID, buyerID).
		Scan(&supplierID, &revokedAt, &consumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if revokedAt == nil && consumedAt == nil {
		if _, err := tx.Exec(ctx, `UPDATE worker_enrollment_codes SET revoked_at=now() WHERE id=$1`, codeID); err != nil {
			return err
		}
		if err := insertWorkerCredentialAuditTx(ctx, tx, "code_revoked", buyerID, supplierID,
			nil, &codeID, nil, "account_revoked", map[string]any{}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) WorkerCredentialAuditForBuyer(ctx context.Context, buyerID uuid.UUID, limit int) ([]WorkerCredentialAuditView, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id,event_type,worker_id,enrollment_code_id,credential_id,
		       COALESCE(reason,''),detail,created_at
		  FROM worker_credential_audit WHERE buyer_id=$1
		 ORDER BY created_at DESC,id DESC LIMIT $2`, buyerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkerCredentialAuditView{}
	for rows.Next() {
		var v WorkerCredentialAuditView
		if err := rows.Scan(&v.ID, &v.EventType, &v.WorkerID, &v.EnrollmentCodeID,
			&v.CredentialID, &v.Reason, &v.Detail, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func decodeEnrollmentJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request must contain one JSON object")
		}
		return err
	}
	return nil
}

func (s *Server) handleCreateWorkerEnrollmentCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	var in EnrollmentCodeIssueInput
	if err := decodeEnrollmentJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid enrollment-code json")
		return
	}
	expectedOrigin, err := enrollmentControlOriginForRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid enrollment-code origin")
		return
	}
	publicKey, _, _, err := parseP256EnrollmentPublicKey(in.DevicePublicKey)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errWorkerEnrollmentInvalid.Error())
		return
	}
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	out, err := s.store.CreateWorkerEnrollmentCode(r.Context(), auth.BuyerID, in, enrollmentRequestBinding{
		Version:       workerEnrollmentProtocolVersion,
		ControlOrigin: expectedOrigin,
		RequestID:     enrollmentRequestID(publicKey),
	})
	if errors.Is(err, errWorkerEnrollmentInvalid) {
		writeErr(w, http.StatusBadRequest, errWorkerEnrollmentInvalid.Error())
		return
	}
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "rotation credential not found or revoked")
		return
	}
	if errors.Is(err, errWorkerEnrollmentDeviceActive) {
		writeErr(w, http.StatusConflict, errWorkerEnrollmentDeviceActive.Error())
		return
	}
	if err != nil {
		writeSupplierStoreError(w, "creating enrollment code", err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleApproveWorkerEnrollmentRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	var in EnrollmentApprovalInput
	if err := decodeEnrollmentJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid enrollment approval json")
		return
	}
	expectedOrigin, err := enrollmentControlOriginForRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid enrollment approval origin")
		return
	}
	request, err := decodeEnrollmentDeviceRequest(in.DeviceRequest, expectedOrigin)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errWorkerEnrollmentInvalid.Error())
		return
	}
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	issued, err := s.store.CreateWorkerEnrollmentCode(r.Context(), auth.BuyerID, EnrollmentCodeIssueInput{
		Audience:           request.Audience,
		DeviceKeyAlgorithm: request.DeviceKeyAlgorithm,
		DevicePublicKey:    request.DevicePublicKey,
		Label:              in.Label,
	}, enrollmentRequestBinding{
		Version:       request.Version,
		ControlOrigin: request.ControlOrigin,
		RequestID:     request.RequestID,
	})
	if errors.Is(err, errWorkerEnrollmentInvalid) {
		writeErr(w, http.StatusBadRequest, errWorkerEnrollmentInvalid.Error())
		return
	}
	if errors.Is(err, errWorkerEnrollmentDeviceActive) {
		writeErr(w, http.StatusConflict, errWorkerEnrollmentDeviceActive.Error())
		return
	}
	if err != nil {
		writeSupplierStoreError(w, "approving enrollment request", err)
		return
	}
	bundle, err := encodeEnrollmentApprovalBundle(request, issued)
	if err != nil {
		log.Printf("encoding enrollment approval bundle failed: %v", err)
		writeErr(w, http.StatusInternalServerError, "encoding enrollment approval bundle failed")
		return
	}
	writeJSON(w, http.StatusCreated, EnrollmentApprovalResult{
		EnrollmentBundle: bundle,
		EnrollmentCodeID: issued.EnrollmentCodeID,
		ExpiresAt:        issued.ExpiresAt,
	})
}

func (s *Server) handleExchangeWorkerEnrollmentCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	var in EnrollmentExchangeInput
	if err := decodeEnrollmentJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid enrollment exchange json")
		return
	}
	out, err := s.store.EnrollWorkerTx(r.Context(), in)
	if errors.Is(err, errWorkerEnrollmentRejected) {
		writeErr(w, http.StatusUnauthorized, "enrollment exchange rejected")
		return
	}
	if err != nil {
		log.Printf("worker enrollment exchange failed: %v", err)
		writeErr(w, http.StatusInternalServerError, "enrollment exchange failed")
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleListWorkerCredentials(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	out, err := s.store.ListWorkerCredentialsForBuyer(r.Context(), auth.BuyerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "listing worker credentials: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

func (s *Server) handleRevokeWorkerCredential(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid credential id")
		return
	}
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	err = s.store.RevokeWorkerCredentialForBuyer(r.Context(), auth.BuyerID, id)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "credential not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "revoking credential: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRevokeWorkerEnrollmentCode(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid enrollment code id")
		return
	}
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	err = s.store.RevokeWorkerEnrollmentCodeForBuyer(r.Context(), auth.BuyerID, id)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "enrollment code not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "revoking enrollment code: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWorkerCredentialAudit(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	out, err := s.store.WorkerCredentialAuditForBuyer(r.Context(), auth.BuyerID, 100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "loading credential audit: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}
