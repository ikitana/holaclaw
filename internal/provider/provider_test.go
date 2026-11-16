package provider

import (
	"encoding/json"
	"testing"
)

func TestParseUSD(t *testing.T) {
	cases := []struct {
		in   string
		want NanoUSD
	}{
		{"0", 0},
		{"0.0", 0},
		{"-0", 0},
		{"0.000123", 123000},
		{"1.2e-05", 12000},
		{"1.2E-05", 12000},
		{"12", 12000000000},
		{"0.000000001", 1},
		{"0.0000000005", 1},  // half rounds away from zero
		{"0.00000000049", 0}, // below half rounds down
		{"0.1234567894999", 123456789},
		{"0.1234567895", 123456790},
		{"1e-200", 0},
		{"9223372036.854775807", 9223372036854775807}, // the largest NanoUSD
	}
	for _, c := range cases {
		got, err := ParseUSD(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseUSD(%q) = %d, %v, want %d", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{
		"", "-0.01", "-1e-9", "abc", "0.1.2", "1e", "null", `"0.1"`, "true",
		"9223372036.854775808", "1e20", "1e200",
		"0.00000000000000000000000000000000000000000000000000000000000000001", // too long
	} {
		if got, err := ParseUSD(bad); err == nil {
			t.Errorf("ParseUSD(%q) = %d, want an error", bad, got)
		}
	}
}

func TestWireCost(t *testing.T) {
	cases := []struct {
		raw  string
		want *NanoUSD
	}{
		{"", nil},
		{"null", nil},
		{`"0.01"`, nil},
		{"-0.5", nil},
		{"{}", nil},
		{"0", ptrNano(0)},
		{"0.000123", ptrNano(123000)},
		{"1.2e-05", ptrNano(12000)},
	}
	for _, c := range cases {
		got := WireCost(json.RawMessage(c.raw))
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("WireCost(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestNanoUSDDecimal(t *testing.T) {
	for n, want := range map[NanoUSD]string{0: "0", 1: "0.000000001", 123000: "0.000123", 1500000000: "1.5"} {
		if got := n.Decimal().String(); got != want {
			t.Errorf("%d.Decimal() = %s, want %s", n, got, want)
		}
	}
}

func ptrNano(v NanoUSD) *NanoUSD { return &v }
