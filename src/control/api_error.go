package main

import (
	"net/http"
	"strconv"
	"strings"
)

// Closed set of stable API error codes. Clients must branch on Code, not on
// the English Error string. Adding a code is a contract change.
type APIErrorCode string

const (
	ErrCodeUnauthorized    APIErrorCode = "unauthorized"
	ErrCodeForbidden       APIErrorCode = "forbidden"
	ErrCodeNotFound        APIErrorCode = "not_found"
	ErrCodeInvalidRequest  APIErrorCode = "invalid_request"
	ErrCodeConflict        APIErrorCode = "conflict"
	ErrCodeRateLimited     APIErrorCode = "rate_limited"
	ErrCodePaymentRequired APIErrorCode = "payment_required"
	ErrCodePayloadTooLarge APIErrorCode = "payload_too_large"
	ErrCodeGone            APIErrorCode = "gone"
	ErrCodeUnavailable     APIErrorCode = "unavailable"     // retryable 503
	ErrCodeMisconfigured   APIErrorCode = "misconfigured"   // non-retryable 503
	ErrCodeOperatorPaused  APIErrorCode = "operator_paused" // retryable pause
	ErrCodeInternal        APIErrorCode = "internal_error"
)

// BuyerAction tells an automated client what to do next without parsing prose.
type BuyerAction string

const (
	ActionAuthenticate   BuyerAction = "authenticate"
	ActionFixRequest     BuyerAction = "fix_request"
	ActionRetry          BuyerAction = "retry"
	ActionRetryAfter     BuyerAction = "retry_after"
	ActionAddPayment     BuyerAction = "add_payment"
	ActionContactSupport BuyerAction = "contact_support"
	ActionNone           BuyerAction = "none"
)

// AllAPIErrorCodes is the closed enum used by tests and documentation.
var AllAPIErrorCodes = []APIErrorCode{
	ErrCodeUnauthorized,
	ErrCodeForbidden,
	ErrCodeNotFound,
	ErrCodeInvalidRequest,
	ErrCodeConflict,
	ErrCodeRateLimited,
	ErrCodePaymentRequired,
	ErrCodePayloadTooLarge,
	ErrCodeGone,
	ErrCodeUnavailable,
	ErrCodeMisconfigured,
	ErrCodeOperatorPaused,
	ErrCodeInternal,
}

var validAPIErrorCodes = func() map[APIErrorCode]bool {
	m := make(map[APIErrorCode]bool, len(AllAPIErrorCodes))
	for _, c := range AllAPIErrorCodes {
		m[c] = true
	}
	return m
}()

// defaultRetryAfterSeconds is used when a 429/retryable-503 path did not set
// a more precise Retry-After already (e.g. login lockout).
const (
	defaultRateLimitRetryAfter   = 1
	defaultUnavailableRetryAfter = 5
	defaultPausedRetryAfter      = 30
)

// classifyAPIError maps HTTP status + message to a stable code, buyer action,
// and optional Retry-After seconds (0 means do not set the header).
func classifyAPIError(status int, msg string) (code APIErrorCode, action BuyerAction, retryAfterSec int) {
	lower := strings.ToLower(msg)

	// Prefer message-derived classes when they are more specific than status.
	switch {
	case strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many") ||
		strings.Contains(lower, "limit reached"):
		if status == http.StatusTooManyRequests || status == 0 {
			return ErrCodeRateLimited, ActionRetryAfter, defaultRateLimitRetryAfter
		}
	case strings.Contains(lower, "payment method") ||
		strings.Contains(lower, "free credit is exhausted") ||
		strings.Contains(lower, "save a card"):
		return ErrCodePaymentRequired, ActionAddPayment, 0
	case strings.Contains(lower, "not configured") ||
		strings.Contains(lower, "is not configured"):
		return ErrCodeMisconfigured, ActionContactSupport, 0
	case strings.Contains(lower, "paused by the operator"):
		return ErrCodeOperatorPaused, ActionRetryAfter, defaultPausedRetryAfter
	}

	switch status {
	case http.StatusUnauthorized:
		return ErrCodeUnauthorized, ActionAuthenticate, 0
	case http.StatusPaymentRequired:
		return ErrCodePaymentRequired, ActionAddPayment, 0
	case http.StatusForbidden:
		return ErrCodeForbidden, ActionNone, 0
	case http.StatusNotFound:
		return ErrCodeNotFound, ActionNone, 0
	case http.StatusConflict:
		return ErrCodeConflict, ActionFixRequest, 0
	case http.StatusGone:
		return ErrCodeGone, ActionNone, 0
	case http.StatusRequestEntityTooLarge:
		return ErrCodePayloadTooLarge, ActionFixRequest, 0
	case http.StatusTooManyRequests:
		return ErrCodeRateLimited, ActionRetryAfter, defaultRateLimitRetryAfter
	case http.StatusServiceUnavailable:
		// Remaining 503s are treated as transient unless message reclassified above.
		return ErrCodeUnavailable, ActionRetryAfter, defaultUnavailableRetryAfter
	case http.StatusBadRequest:
		return ErrCodeInvalidRequest, ActionFixRequest, 0
	case http.StatusInternalServerError:
		return ErrCodeInternal, ActionContactSupport, 0
	default:
		if status >= 500 {
			return ErrCodeUnavailable, ActionRetryAfter, defaultUnavailableRetryAfter
		}
		if status >= 400 {
			return ErrCodeInvalidRequest, ActionFixRequest, 0
		}
		return ErrCodeInternal, ActionContactSupport, 0
	}
}

// writeErr emits the stable APIError envelope. Every call site that uses
// writeErr (including httpError returns) therefore carries a non-empty code
// from the closed enum. Retry-After is set for 429 and genuinely retryable 503s
// when the caller has not already provided a more precise value.
func writeErr(w http.ResponseWriter, status int, msg string) {
	if msg == "" {
		msg = http.StatusText(status)
		if msg == "" {
			msg = "request failed"
		}
	}
	code, action, retryAfter := classifyAPIError(status, msg)
	if !validAPIErrorCodes[code] || code == "" {
		// Defensive: never emit an unknown or empty code.
		code = ErrCodeInternal
		action = ActionContactSupport
		retryAfter = 0
	}
	if retryAfter > 0 && w.Header().Get("Retry-After") == "" {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	writeJSON(w, status, APIError{Error: msg, Code: code, Action: action})
}
