package reviewrequest

import (
	"errors"
	"fmt"
)

// Error codes of the contract (docs/design/contracts/schemas/common.schema.json).
const (
	CodeInvalidArgument     = "INVALID_ARGUMENT"
	CodePermissionDenied    = "PERMISSION_DENIED"
	CodeRequestNotFound     = "REQUEST_NOT_FOUND"
	CodeRequestExpired      = "REQUEST_EXPIRED"
	CodeHeadConflict        = "HEAD_CONFLICT"
	CodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	CodeRevisionConflict    = "REVISION_CONFLICT"
	CodeCancelResultPosted  = "CANCEL_RESULT_POSTED"
	CodeCancelUnconfirmed   = "CANCEL_UNCONFIRMED"
	CodeCandidateCancelled  = "CANDIDATE_CANCELLED"
	CodeOriginRejected      = "ORIGIN_REJECTED"
	CodeDeliveryExpired     = "DELIVERY_EXPIRED"
	CodeEpochStale          = "EPOCH_STALE"
	CodeSequenceConflict    = "SEQUENCE_CONFLICT"
	CodeObservationStale    = "OBSERVATION_STALE"
	CodeProducerNotAssigned = "PRODUCER_NOT_ASSIGNED"
	CodeResultVersionStale  = "RESULT_VERSION_STALE"
	CodeResultHeadMismatch  = "RESULT_HEAD_MISMATCH"
	CodeUnavailable         = "UNAVAILABLE"
)

// retryable lists the only codes a caller may retry: a lost ACK / 5xx-like
// failure, or a cancellation whose GitHub write outcome is still unknown.
var retryable = map[string]bool{CodeUnavailable: true, CodeCancelUnconfirmed: true}

// Error is a structured contract error. Authentication, authorization and
// schema conflicts are returned as-is and never bypassed automatically.
type Error struct {
	Code    string
	Message string
	// Retryable mirrors the contract's error envelope.
	Retryable bool
	Details   map[string]any
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func newError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Retryable: retryable[code]}
}

// deliveryError maps a purged request to DELIVERY_EXPIRED for the internal
// (service-to-service) surfaces: a late redelivery for a removed request is a
// terminal condition for the sender, unlike a public read of an expired URI.
func deliveryError(err error) error {
	if ErrorCode(err) == CodeRequestExpired {
		return newError(CodeDeliveryExpired, "the request was removed after its retention period")
	}
	return err
}

// ErrorCode returns the contract code of err, or "" when err is not an *Error.
func ErrorCode(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return ""
}
