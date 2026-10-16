package money

import (
	"errors"
	"math"
	"testing"
)

func TestString(t *testing.T) {
	tests := map[Amount]string{
		0:             "0.000000",
		1:             "0.000001",
		OneUSDT:       "1.000000",
		98_500_000:    "98.500000",
		-2_500_000:    "-2.500000",
		math.MaxInt64: "9223372036854.775807",
		math.MinInt64: "-9223372036854.775808",
	}
	for in, want := range tests {
		if got := in.String(); got != want {
			t.Errorf("Amount(%d).String() = %q, want %q", int64(in), got, want)
		}
	}
}

func TestParse(t *testing.T) {
	valid := map[string]Amount{
		"0":                    0,
		"1":                    OneUSDT,
		"1.5":                  1_500_000,
		"0.000001":             1,
		"100.250000":           100_250_000,
		"007.10":               7_100_000,
		"9223372036854.775807": math.MaxInt64,
	}
	for in, want := range valid {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", ".", "1.", ".5", "-1", "+1", "1.0000001", "1e6", "1,5", " 1", "abc", "9223372036854.775808", "99999999999999999999"} {
		if _, err := Parse(in); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("Parse(%q) error = %v, want ErrInvalidAmount", in, err)
		}
	}
}
