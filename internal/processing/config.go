package processing

import (
	"errors"
	"fmt"
	"time"
)

type Config struct {
	Confirmations  uint64
	StartBlock     uint64
	ScanBatch      uint64
	BatchSize      int32
	PollInterval   time.Duration
	AMLConcurrency int
	AMLTimeout     time.Duration
	AMLLease       time.Duration
	AMLBackoffBase time.Duration
	AMLBackoffMax  time.Duration
}

func DefaultConfig() Config {
	return Config{
		Confirmations:  5,
		ScanBatch:      500,
		BatchSize:      100,
		PollInterval:   time.Second,
		AMLConcurrency: 8,
		AMLTimeout:     10 * time.Second,
		AMLLease:       30 * time.Second,
		AMLBackoffBase: time.Second,
		AMLBackoffMax:  5 * time.Minute,
	}
}

func (c Config) validate() error {
	var errs []error
	if c.Confirmations == 0 {
		errs = append(errs, errors.New("confirmations must be at least 1"))
	}
	if c.ScanBatch == 0 {
		errs = append(errs, errors.New("ScanBatch must be at least 1"))
	}
	if c.BatchSize <= 0 {
		errs = append(errs, errors.New("BatchSize must be positive"))
	}
	if c.PollInterval <= 0 {
		errs = append(errs, errors.New("PollInterval must be positive"))
	}
	if c.AMLConcurrency <= 0 {
		errs = append(errs, errors.New("AMLConcurrency must be positive"))
	}
	if c.AMLTimeout <= 0 {
		errs = append(errs, errors.New("AMLTimeout must be positive"))
	}
	if c.AMLLease <= c.AMLTimeout {
		errs = append(errs, errors.New("AMLLease must be longer than AMLTimeout"))
	}
	if c.AMLBackoffBase <= 0 {
		errs = append(errs, errors.New("AMLBackoffBase must be positive"))
	}
	if c.AMLBackoffMax < c.AMLBackoffBase {
		errs = append(errs, errors.New("AMLBackoffMax must not be less than AMLBackoffBase"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("processing: invalid config: %w", err)
	}
	return nil
}
