package budget

import "testing"

// An operator reading a denial needs to recognize the limit they set.
// Rendering every amount to two decimals turns a deliberately tiny
// budget into "0.00", which reads as misconfigured rather than small.
func TestFormatUSD(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want string
	}{
		{"whole dollars", 5, "5.00"},
		{"cents", 1.25, "1.25"},
		{"exactly one cent", 0.01, "0.01"},
		{"sub cent keeps its digits", 0.000001, "1e-06"},
		{"small but readable", 0.005, "0.005"},
		{"zero", 0, "0.00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatUSD(tc.in); got != tc.want {
				t.Errorf("formatUSD(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
