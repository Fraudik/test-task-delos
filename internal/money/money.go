package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

type Amount int64

// OneUSDT is 1 USDT in micro-units (wei)
const OneUSDT Amount = 1_000_000

const Decimals = 6

var ErrInvalidAmount = errors.New("money: invalid amount")

func (a Amount) String() string {
	sign := ""
	u := uint64(a)
	if a < 0 {
		sign = "-"
		u = -u
	}
	unit := uint64(OneUSDT)
	return fmt.Sprintf("%s%d.%0*d", sign, u/unit, Decimals, u%unit)
}

func Parse(s string) (Amount, error) {
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" || len(frac) > Decimals || (hasFrac && frac == "") || !digits(whole) || !digits(frac) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > math.MaxInt64/int64(OneUSDT) {
		return 0, fmt.Errorf("%w: %q is too large", ErrInvalidAmount, s)
	}
	f := int64(0)
	if frac != "" {
		f, _ = strconv.ParseInt(frac+strings.Repeat("0", Decimals-len(frac)), 10, 64)
	}
	total := w*int64(OneUSDT) + f
	if total < 0 {
		return 0, fmt.Errorf("%w: %q is too large", ErrInvalidAmount, s)
	}
	return Amount(total), nil
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
