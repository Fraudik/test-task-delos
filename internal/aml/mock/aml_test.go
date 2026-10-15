package mock

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"test-task-dellos/internal/aml"
	"test-task-dellos/internal/money"
)

// Проверка соответствия интерфейсу
var _ aml.Checker = (*Mock)(nil)

const badAddr = "0xbad0000000000000000000000000000000000bad"

func request(from, to string) aml.Request {
	return aml.Request{TxHash: "0xtx", From: from, To: to, Amount: money.OneUSDT}
}

func TestCheckLowRisk(t *testing.T) {
	for name, s := range map[string]*Mock{
		"nil blacklist":   New(nil),
		"other addresses": New([]string{badAddr}),
	} {
		t.Run(name, func(t *testing.T) {
			res, err := s.Check(t.Context(), request("0xgood", "0xdeposit"))
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if res.Risk != aml.RiskLow {
				t.Errorf("risk = %q, want %q", res.Risk, aml.RiskLow)
			}
			if res.Reason != "" {
				t.Errorf("reason = %q, want empty", res.Reason)
			}
		})
	}
}

func TestCheckBlacklistedSender(t *testing.T) {
	tests := []struct {
		name      string
		blacklist string
		from      string
	}{
		{"exact match", badAddr, badAddr},
		{"request in upper case", badAddr, "0xBAD0000000000000000000000000000000000BAD"},
		{"blacklist with spaces", "  " + badAddr + "\n", badAddr},
		{"mixed cases", " 0xBAD0000000000000000000000000000000000bad", "0xbad0000000000000000000000000000000000BAD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New([]string{"0xother", tt.blacklist})
			res, err := s.Check(t.Context(), request(tt.from, "0xdeposit"))
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if res.Risk != aml.RiskHigh {
				t.Errorf("risk = %q, want %q", res.Risk, aml.RiskHigh)
			}
			if res.Reason == "" {
				t.Error("empty reason for high risk")
			}
		})
	}
}

func TestCheckContextDone(t *testing.T) {
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		wantErr error
	}{
		{"cancelled", cancelled, context.Canceled},
		{"deadline exceeded", expired, context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New([]string{badAddr})
			res, err := s.Check(tt.ctx, request(badAddr, "0xdeposit"))
			if !errors.Is(err, aml.ErrUnavailable) {
				t.Errorf("error = %v, want ErrUnavailable", err)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
			if res != (aml.Result{}) {
				t.Errorf("result = %+v, want zero", res)
			}
		})
	}
}

func TestAvailability(t *testing.T) {
	s := New([]string{badAddr})

	if !s.Available() {
		t.Fatal("new Mock is unavailable")
	}

	s.SetAvailability(false)
	if s.Available() {
		t.Error("Available() = true after SetAvailability(false)")
	}
	for _, from := range []string{"0xgood", badAddr} {
		res, err := s.Check(t.Context(), request(from, "0xdeposit"))
		if !errors.Is(err, aml.ErrUnavailable) {
			t.Errorf("Check(%s) error = %v, want ErrUnavailable", from, err)
		}
		if res != (aml.Result{}) {
			t.Errorf("Check(%s) result = %+v while down, want zero", from, res)
		}
	}

	s.SetAvailability(true)
	if !s.Available() {
		t.Error("Available() = false after SetAvailability(true)")
	}
	res, err := s.Check(t.Context(), request(badAddr, "0xdeposit"))
	if err != nil {
		t.Fatalf("Check after recovery: %v", err)
	}
	if res.Risk != aml.RiskHigh {
		t.Errorf("risk after recovery = %q, want %q", res.Risk, aml.RiskHigh)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := New([]string{badAddr})
	var wg sync.WaitGroup

	wg.Go(func() {
		for i := range 100 {
			s.SetAvailability(i%2 == 0)
			s.Available()
		}
	})
	for range 4 {
		wg.Go(func() {
			for range 100 {
				res, err := s.Check(t.Context(), request(badAddr, "0xdeposit"))
				if err != nil {
					if !errors.Is(err, aml.ErrUnavailable) {
						t.Errorf("unexpected error: %v", err)
					}
					continue
				}
				if res.Risk != aml.RiskHigh {
					t.Errorf("risk = %q, want %q", res.Risk, aml.RiskHigh)
				}
			}
		})
	}
	wg.Wait()
}
