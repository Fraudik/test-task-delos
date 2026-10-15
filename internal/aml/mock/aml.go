package mock

import (
	"context"
	"errors"
	"strings"
	"sync"

	"test-task-dellos/internal/aml"
)

type Mock struct {
	mu        sync.RWMutex
	blacklist map[string]struct{}
	down      bool
}

func normalize(addr string) string {
	return strings.ToLower(strings.TrimSpace(addr))
}

func New(blacklist []string) *Mock {
	s := &Mock{blacklist: make(map[string]struct{}, len(blacklist))}
	for _, a := range blacklist {
		if a = normalize(a); a != "" {
			s.blacklist[a] = struct{}{}
		}
	}
	return s
}

func (s *Mock) SetAvailability(availability bool) {
	s.mu.Lock()
	s.down = !availability
	s.mu.Unlock()
}

func (s *Mock) Available() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.down
}

func (s *Mock) Check(ctx context.Context, req aml.Request) (aml.Result, error) {
	if err := ctx.Err(); err != nil {
		return aml.Result{}, errors.Join(aml.ErrUnavailable, err)
	}

	s.mu.RLock()
	down := s.down
	_, blacklisted := s.blacklist[normalize(req.From)]
	s.mu.RUnlock()

	if down {
		return aml.Result{}, aml.ErrUnavailable
	}
	if blacklisted {
		return aml.Result{Risk: aml.RiskHigh, Reason: "sender address is blacklisted"}, nil
	}
	return aml.Result{Risk: aml.RiskLow}, nil
}
