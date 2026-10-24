package reviewrequest

import (
	"fmt"
	"time"
)

const day = 24 * time.Hour

// Settings holds the retention, delivery and observation parameters. Defaults
// and lower bounds follow docs/design/contracts (settings.schema.json).
type Settings struct {
	// Retention applies to finished (result / superseded / cancelled) requests,
	// the idempotency ledger and cancellation records. The cancellation record
	// must outlive the redelivery window of an old acceptance.
	FinishedRequestRetention    time.Duration
	IdempotencyRetention        time.Duration
	CancellationRecordRetention time.Duration

	Delivery    DeliverySettings
	Observation ObservationSettings
}

// DeliverySettings bounds retries toward the reviewer side. Only lost ACKs and
// 5xx-like failures are retried; authorization and schema conflicts never are.
type DeliverySettings struct {
	MaxAttempts       int
	InitialBackoff    time.Duration
	BackoffMultiplier float64
	MaxBackoff        time.Duration
	JitterRatio       float64
}

// ObservationSettings governs freshness of execution observations.
type ObservationSettings struct {
	Heartbeat      time.Duration
	StaleAfter     time.Duration
	ClockSkewLimit time.Duration
}

// DefaultSettings returns the contract defaults.
func DefaultSettings() Settings {
	return Settings{
		FinishedRequestRetention:    30 * day,
		IdempotencyRetention:        30 * day,
		CancellationRecordRetention: 30 * day,
		Delivery: DeliverySettings{
			MaxAttempts:       8,
			InitialBackoff:    time.Second,
			BackoffMultiplier: 2,
			MaxBackoff:        time.Minute,
			JitterRatio:       0.2,
		},
		Observation: ObservationSettings{
			Heartbeat:      time.Minute,
			StaleAfter:     3 * time.Minute,
			ClockSkewLimit: 30 * time.Second,
		},
	}
}

// Validate rejects values below the contract's lower bounds so a misconfigured
// deployment fails at startup instead of silently shortening retention.
func (s Settings) Validate() error {
	for name, d := range map[string]time.Duration{
		"finished request retention":    s.FinishedRequestRetention,
		"idempotency retention":         s.IdempotencyRetention,
		"cancellation record retention": s.CancellationRecordRetention,
	} {
		if d < 30*day {
			return fmt.Errorf("%s must be at least 30 days, got %s", name, d)
		}
	}
	d := s.Delivery
	switch {
	case d.MaxAttempts < 1 || d.MaxAttempts > 20:
		return fmt.Errorf("delivery max attempts must be within 1..20, got %d", d.MaxAttempts)
	case d.InitialBackoff < 100*time.Millisecond || d.MaxBackoff < 100*time.Millisecond:
		return fmt.Errorf("delivery backoff must be at least 100ms")
	case d.BackoffMultiplier < 1:
		return fmt.Errorf("delivery backoff multiplier must be at least 1, got %v", d.BackoffMultiplier)
	case d.JitterRatio < 0 || d.JitterRatio > 1:
		return fmt.Errorf("delivery jitter ratio must be within 0..1, got %v", d.JitterRatio)
	}
	o := s.Observation
	switch {
	case o.Heartbeat < time.Second:
		return fmt.Errorf("observation heartbeat must be at least 1s, got %s", o.Heartbeat)
	case o.StaleAfter < 2*time.Minute:
		return fmt.Errorf("observation stale-after must be at least 2m, got %s", o.StaleAfter)
	case o.ClockSkewLimit < 0 || o.ClockSkewLimit > 5*time.Minute:
		return fmt.Errorf("observation clock skew limit must be within 0..5m, got %s", o.ClockSkewLimit)
	}
	return nil
}
