package aml

import (
	"context"
	"errors"
	"math/big"
)

var ErrUnavailable = errors.New("aml: provider unavailable")

type Request struct {
	TxHash string
	From   string
	To     string
	Amount *big.Int
}

type Risk string

const (
	RiskLow  Risk = "low"
	RiskHigh Risk = "high"
)

type Result struct {
	Risk   Risk
	Reason string
}

type Checker interface {
	Check(ctx context.Context, req Request) (Result, error)
}
