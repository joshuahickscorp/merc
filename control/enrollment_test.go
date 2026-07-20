package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type enrollmentTestKey struct {
	private *ecdsa.PrivateKey
	encoded string
}

func newEnrollmentTestKey(t *testing.T) enrollmentTestKey {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := private.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return enrollmentTestKey{
		private: private,
		encoded: base64.RawURLEncoding.EncodeToString(raw),
	}
}

func signEnrollmentTestProof(t *testing.T, key *ecdsa.PrivateKey, transcript []byte) string {
	t.Helper()
	digest := sha256.Sum256(transcript)
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(signature)
}

func TestP256EnrollmentPublicKeyCanonicalizesAndFingerprints(t *testing.T) {
	key := newEnrollmentTestKey(t)
	raw, parsed, fingerprint, err := parseP256EnrollmentPublicKey(key.encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.RawURLEncoding.EncodeToString(raw); got != key.encoded {
		t.Fatalf("canonical key changed: want %q, got %q", key.encoded, got)
	}
	parsedBytes, err := parsed.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := key.private.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(parsedBytes, wantBytes) {
		t.Fatal("parsed public key differs from generated key")
	}
	if !strings.HasPrefix(fingerprint, "p256:sha256:") || len(fingerprint) != len("p256:sha256:")+64 {
		t.Fatalf("unexpected fingerprint %q", fingerprint)
	}

	_, _, sameFingerprint, err := parseP256EnrollmentPublicKey(key.encoded)
	if err != nil || sameFingerprint != fingerprint {
		t.Fatalf("fingerprint must be deterministic: got %q, %v", sameFingerprint, err)
	}
}

func TestP256EnrollmentPublicKeyRejectsNonCanonicalAndInvalidPoints(t *testing.T) {
	key := newEnrollmentTestKey(t)
	raw, err := base64.RawURLEncoding.DecodeString(key.encoded)
	if err != nil {
		t.Fatal(err)
	}
	invalidPoint := append([]byte(nil), raw...)
	for i := 1; i < len(invalidPoint); i++ {
		invalidPoint[i] = 0
	}

	cases := []string{
		"",
		" " + key.encoded,
		key.encoded + "=",
		base64.RawURLEncoding.EncodeToString(raw[:64]),
		base64.RawURLEncoding.EncodeToString(append([]byte{2}, raw[1:]...)),
		base64.RawURLEncoding.EncodeToString(invalidPoint),
	}
	for _, encoded := range cases {
		if _, _, _, err := parseP256EnrollmentPublicKey(encoded); err == nil {
			t.Fatalf("accepted invalid public key %q", encoded)
		}
	}
}

func TestEnrollmentProofBindsCodeAudienceAndAccount(t *testing.T) {
	key := newEnrollmentTestKey(t)
	_, parsed, _, err := parseP256EnrollmentPublicKey(key.encoded)
	if err != nil {
		t.Fatal(err)
	}
	accountID := uuid.New()
	code := newSecret(workerEnrollmentCodePrefix)
	origin := "https://control.example.test"
	publicKey, err := key.private.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	requestID := enrollmentRequestID(publicKey)
	transcript := enrollmentExchangeTranscript(code, workerEnrollmentAudience, accountID, origin, requestID)
	proof := signEnrollmentTestProof(t, key.private, transcript)
	if !verifyEnrollmentProof(parsed, proof, transcript) {
		t.Fatal("valid enrollment proof rejected")
	}

	wrongCases := [][]byte{
		enrollmentExchangeTranscript(newSecret(workerEnrollmentCodePrefix), workerEnrollmentAudience, accountID, origin, requestID),
		enrollmentExchangeTranscript(code, "cx-wrong-audience", accountID, origin, requestID),
		enrollmentExchangeTranscript(code, workerEnrollmentAudience, uuid.New(), origin, requestID),
		enrollmentExchangeTranscript(code, workerEnrollmentAudience, accountID, "https://relay.example.test", requestID),
		enrollmentExchangeTranscript(code, workerEnrollmentAudience, accountID, origin, strings.Repeat("A", 22)),
		append(append([]byte(nil), transcript...), '\n'),
	}
	for _, wrong := range wrongCases {
		if verifyEnrollmentProof(parsed, proof, wrong) {
			t.Fatalf("proof accepted for altered transcript %q", wrong)
		}
	}
	if verifyEnrollmentProof(parsed, "not-base64!", transcript) {
		t.Fatal("malformed proof accepted")
	}
}

func TestEnrollmentCodeFormatRequiresPrefixAndThirtyTwoBytes(t *testing.T) {
	valid := newSecret(workerEnrollmentCodePrefix)
	if !validEnrollmentCode(valid) {
		t.Fatalf("generated enrollment code rejected: %q", valid)
	}
	cases := []string{
		"",
		strings.TrimPrefix(valid, workerEnrollmentCodePrefix),
		"cxw_" + strings.TrimPrefix(valid, workerEnrollmentCodePrefix),
		valid + " ",
		workerEnrollmentCodePrefix + base64.RawURLEncoding.EncodeToString(make([]byte, 31)),
		workerEnrollmentCodePrefix + base64.RawURLEncoding.EncodeToString(make([]byte, 33)),
	}
	for _, code := range cases {
		if validEnrollmentCode(code) {
			t.Fatalf("accepted invalid enrollment code %q", code)
		}
	}
}

func encodeEnrollmentDeviceRequestTest(t *testing.T, payload any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return workerEnrollmentRequestPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func enrollmentDeviceRequestPayload(t *testing.T, key enrollmentTestKey, origin string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(key.encoded)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"v":                    workerEnrollmentProtocolVersion,
		"control_origin":       origin,
		"audience":             workerEnrollmentAudience,
		"request_id":           enrollmentRequestID(raw),
		"device_key_algorithm": workerEnrollmentKeyAlgorithm,
		"device_public_key":    key.encoded,
	}
}

func TestEnrollmentDeviceRequestStrictlyBindsSwiftWireFields(t *testing.T) {
	key := newEnrollmentTestKey(t)
	origin := "https://control.example.test"
	payload := enrollmentDeviceRequestPayload(t, key, origin)
	encoded := encodeEnrollmentDeviceRequestTest(t, payload)
	request, err := decodeEnrollmentDeviceRequest(" \n"+encoded+"\n", origin)
	if err != nil {
		t.Fatalf("decode canonical request: %v", err)
	}
	if request.Version != workerEnrollmentProtocolVersion || request.ControlOrigin != origin ||
		request.Audience != workerEnrollmentAudience ||
		request.DeviceKeyAlgorithm != workerEnrollmentKeyAlgorithm ||
		request.DevicePublicKey != key.encoded || request.RequestID != payload["request_id"] {
		t.Fatalf("decoded request changed wire binding: %+v", request)
	}

	stableKey := "BGsX0fLhLEJH-Lzm5WOkQPJ3A32BLeszoPShOUXYmMKWT-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU" // gitleaks:allow -- fixed public-key fixture, not a credential
	stableRaw, err := base64.RawURLEncoding.DecodeString(stableKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := enrollmentRequestID(stableRaw); got != "ptTtmFOpwVsIlLyS0VBcnQ" {
		t.Fatalf("Swift/Go request id drift: got %q", got)
	}
}

func TestEnrollmentDeviceRequestRejectsMalformedTamperedAndCrossOriginInputs(t *testing.T) {
	key := newEnrollmentTestKey(t)
	origin := "https://control.example.test"
	validPayload := enrollmentDeviceRequestPayload(t, key, origin)
	validJSON, err := json.Marshal(validPayload)
	if err != nil {
		t.Fatal(err)
	}

	mutated := func(field string, value any) string {
		payload := enrollmentDeviceRequestPayload(t, key, origin)
		payload[field] = value
		return encodeEnrollmentDeviceRequestTest(t, payload)
	}
	missing := enrollmentDeviceRequestPayload(t, key, origin)
	delete(missing, "request_id")
	extra := enrollmentDeviceRequestPayload(t, key, origin)
	extra["account_id"] = uuid.NewString()
	duplicateJSON := append(append([]byte(nil), validJSON[:len(validJSON)-1]...),
		[]byte(`,"request_id":"AAAAAAAAAAAAAAAAAAAAAA"}`)...)
	invalidPoint := base64.RawURLEncoding.EncodeToString(append([]byte{4}, make([]byte, 64)...))

	cases := map[string]string{
		"empty":                  "",
		"wrong prefix":           "cxeb1_" + strings.TrimPrefix(encodeEnrollmentDeviceRequestTest(t, validPayload), workerEnrollmentRequestPrefix),
		"bad base64":             workerEnrollmentRequestPrefix + "not+base64",
		"padded base64":          encodeEnrollmentDeviceRequestTest(t, validPayload) + "=",
		"bad json":               workerEnrollmentRequestPrefix + base64.RawURLEncoding.EncodeToString([]byte(`{"v":`)),
		"array":                  encodeEnrollmentDeviceRequestTest(t, []any{}),
		"missing key":            encodeEnrollmentDeviceRequestTest(t, missing),
		"extra identity key":     encodeEnrollmentDeviceRequestTest(t, extra),
		"duplicate key":          workerEnrollmentRequestPrefix + base64.RawURLEncoding.EncodeToString(duplicateJSON),
		"wrong version":          mutated("v", 1),
		"wrong audience":         mutated("audience", "cx-other-agent-v1"),
		"wrong algorithm":        mutated("device_key_algorithm", "ed25519"),
		"tampered request id":    mutated("request_id", "AAAAAAAAAAAAAAAAAAAAAA"),
		"wrong origin":           mutated("control_origin", "https://other.example.test"),
		"noncanonical origin":    mutated("control_origin", origin+"/"),
		"insecure remote origin": mutated("control_origin", "http://control.example.test"),
		"invalid device point":   mutated("device_public_key", invalidPoint),
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeEnrollmentDeviceRequest(encoded, origin); !errors.Is(err, errWorkerEnrollmentInvalid) {
				t.Fatalf("want invalid enrollment request, got %v", err)
			}
		})
	}
}

func TestEnrollmentControlOriginAllowsOnlyCanonicalHTTPSOrExplicitLoopback(t *testing.T) {
	tests := []struct {
		raw       string
		allowHTTP bool
		want      string
		wantErr   bool
	}{
		{"https://CONTROL.Example.Test/", false, "https://control.example.test", false},
		{"http://127.0.0.1:8080", true, "http://127.0.0.1:8080", false},
		{"http://localhost:8080", false, "", true},
		{"http://control.example.test", true, "", true},
		{"https://control.example.test/path", false, "", true},
		{"https://user@control.example.test", false, "", true},
		{"https://control.example.test?x=1", false, "", true},
	}
	for _, tt := range tests {
		got, err := canonicalEnrollmentControlOrigin(tt.raw, tt.allowHTTP)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Fatalf("canonical origin %q: got %q, %v; want %q err=%v", tt.raw, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestEnrollmentApprovalOriginUsesTrustedConfigurationOrDirectTransportOnly(t *testing.T) {
	t.Run("configured origin defeats spoofed host and proxy header", func(t *testing.T) {
		t.Setenv(publicControlOriginEnv, "https://control.example.test")
		r := httptest.NewRequest("POST", "http://attacker.invalid/v1/supplier/enrollment-approvals", nil)
		r.Host = "attacker.invalid"
		r.RemoteAddr = "203.0.113.9:43123"
		r.Header.Set("X-Forwarded-Proto", "https://attacker.invalid")
		got, err := enrollmentControlOriginForRequest(r)
		if err != nil || got != "https://control.example.test" {
			t.Fatalf("configured origin = %q, %v", got, err)
		}
	})

	t.Run("configured origin must be exact HTTPS", func(t *testing.T) {
		r := httptest.NewRequest("POST", "http://127.0.0.1/enroll", nil)
		r.RemoteAddr = "127.0.0.1:43123"
		for _, invalid := range []string{
			"http://control.example.test",
			"https://control.example.test/",
			" https://control.example.test",
		} {
			t.Setenv(publicControlOriginEnv, invalid)
			if _, err := enrollmentControlOriginForRequest(r); err == nil {
				t.Fatalf("accepted invalid configured origin %q", invalid)
			}
		}
	})

	t.Run("unconfigured remote direct TLS is rejected", func(t *testing.T) {
		t.Setenv(publicControlOriginEnv, "")
		r := httptest.NewRequest("POST", "https://Control.Example.Test/enroll", nil)
		r.TLS = &tls.ConnectionState{}
		r.RemoteAddr = "203.0.113.9:43123"
		r.Header.Set("X-Forwarded-Proto", "http")
		if got, err := enrollmentControlOriginForRequest(r); err == nil {
			t.Fatalf("unconfigured direct TLS trusted client Host as %q", got)
		}
	})

	t.Run("loopback direct TLS remains available for development", func(t *testing.T) {
		t.Setenv(publicControlOriginEnv, "")
		r := httptest.NewRequest("POST", "https://localhost:8443/enroll", nil)
		r.TLS = &tls.ConnectionState{}
		r.RemoteAddr = "127.0.0.1:43123"
		got, err := enrollmentControlOriginForRequest(r)
		if err != nil || got != "https://localhost:8443" {
			t.Fatalf("loopback TLS origin = %q, %v", got, err)
		}
	})

	t.Run("loopback HTTP ignores spoofed forwarded proto", func(t *testing.T) {
		t.Setenv(publicControlOriginEnv, "")
		r := httptest.NewRequest("POST", "http://127.0.0.1:8080/enroll", nil)
		r.RemoteAddr = "127.0.0.1:43123"
		r.Header.Set("X-Forwarded-Proto", "https")
		got, err := enrollmentControlOriginForRequest(r)
		if err != nil || got != "http://127.0.0.1:8080" {
			t.Fatalf("loopback origin = %q, %v", got, err)
		}
	})

	t.Run("remote caller cannot spoof loopback host or proxy TLS", func(t *testing.T) {
		t.Setenv(publicControlOriginEnv, "")
		for _, target := range []string{"http://localhost/enroll", "http://control.example.test/enroll"} {
			r := httptest.NewRequest("POST", target, nil)
			r.RemoteAddr = "203.0.113.9:43123"
			r.Header.Set("X-Forwarded-Proto", "https")
			if _, err := enrollmentControlOriginForRequest(r); err == nil {
				t.Fatalf("remote request with target %q manufactured a trusted origin", target)
			}
		}
	})

	t.Run("loopback peer cannot spoof a public host", func(t *testing.T) {
		t.Setenv(publicControlOriginEnv, "")
		r := httptest.NewRequest("POST", "http://control.example.test/enroll", nil)
		r.RemoteAddr = "127.0.0.1:43123"
		if got, err := enrollmentControlOriginForRequest(r); err == nil {
			t.Fatalf("loopback peer manufactured public origin %q", got)
		}
	})
}

func TestEnrollmentApprovalBundleMatchesMacDecoderSchema(t *testing.T) {
	key := newEnrollmentTestKey(t)
	origin := "https://control.example.test"
	requestPayload := enrollmentDeviceRequestPayload(t, key, origin)
	request, err := decodeEnrollmentDeviceRequest(encodeEnrollmentDeviceRequestTest(t, requestPayload), origin)
	if err != nil {
		t.Fatal(err)
	}
	accountID := uuid.MustParse("12345678-1234-4abc-8def-1234567890ab")
	codeID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	code := workerEnrollmentCodePrefix + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	issued := EnrollmentCodeIssueResult{
		EnrollmentCodeID:  codeID,
		EnrollmentCode:    code,
		Audience:          workerEnrollmentAudience,
		AccountID:         accountID,
		SupplierID:        uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"),
		DeviceFingerprint: "p256:sha256:" + strings.Repeat("a", 64),
		ExpiresAt:         time.Unix(1_700_000_000, 0).UTC(),
	}
	bundle, err := encodeEnrollmentApprovalBundle(request, issued)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(bundle, workerEnrollmentBundlePrefix) {
		t.Fatalf("bundle prefix = %q", bundle)
	}
	payloadText := strings.TrimPrefix(bundle, workerEnrollmentBundlePrefix)
	payload, err := base64.RawURLEncoding.DecodeString(payloadText)
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != payloadText {
		t.Fatalf("bundle payload is not canonical base64url: %v", err)
	}
	var decoded enrollmentApprovalBundlePayload
	if err := decodeExactEnrollmentObject(payload, &decoded,
		"v", "control_origin", "account_id", "audience", "enrollment_code", "request_id", "device_fingerprint"); err != nil {
		t.Fatalf("bundle does not match Swift decoder schema: %v", err)
	}
	if decoded.Version != workerEnrollmentProtocolVersion || decoded.ControlOrigin != origin || decoded.AccountID != accountID ||
		decoded.Audience != workerEnrollmentAudience || decoded.EnrollmentCode != code ||
		decoded.RequestID != request.RequestID || decoded.DeviceFingerprint != issued.DeviceFingerprint {
		t.Fatalf("bundle changed approved binding: %+v", decoded)
	}
	if strings.Contains(string(payload), "supplier_id") || strings.Contains(string(payload), issued.SupplierID.String()) {
		t.Fatalf("approval bundle leaked supplier identity: %s", payload)
	}
}

type enrollmentWireFixture struct {
	Version            int    `json:"version"`
	CXER2              string `json:"cxer2"`
	CXEB2              string `json:"cxeb2"`
	ControlOrigin      string `json:"control_origin"`
	AccountID          string `json:"account_id"`
	Audience           string `json:"audience"`
	DeviceKeyAlgorithm string `json:"device_key_algorithm"`
	DevicePublicKey    string `json:"device_public_key"`
	RequestID          string `json:"request_id"`
	DeviceFingerprint  string `json:"device_fingerprint"`
	EnrollmentCode     string `json:"enrollment_code"`
	ExchangeTranscript string `json:"exchange_transcript"`
}

func loadEnrollmentWireFixture(t *testing.T) enrollmentWireFixture {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate enrollment test source")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "proto", "enrollment-wire-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture enrollmentWireFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestEnrollmentWireFixtureBridgesSwiftRequestAndServerBundleByteExactly(t *testing.T) {
	fixture := loadEnrollmentWireFixture(t)
	if fixture.Version != workerEnrollmentProtocolVersion {
		t.Fatalf("fixture version = %d", fixture.Version)
	}
	request, err := decodeEnrollmentDeviceRequest(fixture.CXER2, fixture.ControlOrigin)
	if err != nil {
		t.Fatalf("server rejected byte-exact Swift cxer2 fixture: %v", err)
	}
	if request.Audience != fixture.Audience || request.DeviceKeyAlgorithm != fixture.DeviceKeyAlgorithm ||
		request.DevicePublicKey != fixture.DevicePublicKey || request.RequestID != fixture.RequestID {
		t.Fatalf("Swift fixture binding changed: %+v", request)
	}
	accountID, err := uuid.Parse(fixture.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := encodeEnrollmentApprovalBundle(request, EnrollmentCodeIssueResult{
		EnrollmentCode:    fixture.EnrollmentCode,
		Audience:          fixture.Audience,
		AccountID:         accountID,
		DeviceFingerprint: fixture.DeviceFingerprint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bundle != fixture.CXEB2 {
		t.Fatalf("server cxeb2 encoding drifted from shared fixture:\nwant %s\n got %s", fixture.CXEB2, bundle)
	}
	if got := string(enrollmentExchangeTranscript(fixture.EnrollmentCode, fixture.Audience,
		accountID, fixture.ControlOrigin, fixture.RequestID)); got != fixture.ExchangeTranscript {
		t.Fatalf("server v2 exchange transcript drifted:\nwant %q\n got %q", fixture.ExchangeTranscript, got)
	}
}
