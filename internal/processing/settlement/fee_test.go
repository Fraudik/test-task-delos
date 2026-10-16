package settlement

import (
	"math"
	"testing"

	"test-task-dellos/internal/money"
)

func TestSplitFee(t *testing.T) {
	tests := []struct {
		name     string
		amount   money.Amount
		bps      int32
		fixed    money.Amount
		net, fee money.Amount
	}{
		{"percent only", 100 * money.OneUSDT, 150, 0, 98_500_000, 1_500_000},
		{"no fee", 100 * money.OneUSDT, 0, 0, 100 * money.OneUSDT, 0},
		{"full percent", 100 * money.OneUSDT, 10_000, 0, 0, 100 * money.OneUSDT},
		{"percent rounds down", 1, 9_999, 0, 1, 0},
		{"percent rounds down 2", 3, 3_333, 0, 3, 0},
		{"odd amount", 10_001, 5_000, 0, 5_001, 5_000},
		{"max amount full percent", math.MaxInt64, 10_000, 0, 0, math.MaxInt64},
		{"max amount one bps", math.MaxInt64, 1, 0, math.MaxInt64 - 922_337_203_685_477, 922_337_203_685_477},
		{"fixed only", 100 * money.OneUSDT, 0, 500_000, 99_500_000, 500_000},
		{"fixed plus percent", 100 * money.OneUSDT, 100, 500_000, 98_500_000, 1_500_000},
		{"fixed equals remainder", 10 * money.OneUSDT, 1_000, 9 * money.OneUSDT, 0, 10 * money.OneUSDT},
		{"fixed exceeds amount", 300_000, 0, 500_000, 0, 300_000},
		{"fixed exceeds remainder", money.OneUSDT, 5_000, 600_000, 0, money.OneUSDT},
		{"fixed just below remainder", money.OneUSDT, 5_000, 499_999, 1, 999_999},
		{"max fixed", money.OneUSDT, 100, math.MaxInt64, 0, money.OneUSDT},
		{"max amount with fixed", math.MaxInt64, 1, money.OneUSDT, math.MaxInt64 - 922_337_203_685_477 - money.OneUSDT, 922_337_203_685_477 + money.OneUSDT},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			net, fee := splitFee(tt.amount, tt.bps, tt.fixed)
			if net != tt.net || fee != tt.fee {
				t.Errorf("splitFee(%d, %d, %d) = %d, %d; want %d, %d", tt.amount, tt.bps, tt.fixed, net, fee, tt.net, tt.fee)
			}
			if net+fee != tt.amount || net < 0 || fee < 0 {
				t.Errorf("splitFee(%d, %d, %d): net %d + fee %d != amount", tt.amount, tt.bps, tt.fixed, net, fee)
			}
		})
	}
}
