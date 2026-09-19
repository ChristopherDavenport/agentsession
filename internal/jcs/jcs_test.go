package jcs

import (
	"encoding/hex"
	"math"
	"strconv"
	"testing"
)

// TestFormatNumber uses the vectors from RFC 8785 appendix B: the IEEE
// 754 bit pattern of each input and its expected ECMAScript rendering.
func TestFormatNumber(t *testing.T) {
	tests := []struct {
		bits string
		want string
	}{
		{"0000000000000000", "0"},
		{"8000000000000000", "0"},
		{"0000000000000001", "5e-324"},
		{"8000000000000001", "-5e-324"},
		{"7fefffffffffffff", "1.7976931348623157e+308"},
		{"ffefffffffffffff", "-1.7976931348623157e+308"},
		{"4340000000000000", "9007199254740992"},
		{"c340000000000000", "-9007199254740992"},
		{"4430000000000000", "295147905179352830000"},
		{"44b52d02c7e14af5", "9.999999999999997e+22"},
		{"44b52d02c7e14af6", "1e+23"},
		{"44b52d02c7e14af7", "1.0000000000000001e+23"},
		{"444b1ae4d6e2ef4e", "999999999999999700000"},
		{"444b1ae4d6e2ef4f", "999999999999999900000"},
		{"444b1ae4d6e2ef50", "1e+21"},
		{"3eb0c6f7a0b5ed8c", "9.999999999999997e-7"},
		{"3eb0c6f7a0b5ed8d", "0.000001"},
		{"41b3de4355555553", "333333333.3333332"},
		{"41b3de4355555554", "333333333.33333325"},
		{"41b3de4355555555", "333333333.3333333"},
		{"41b3de4355555556", "333333333.3333334"},
		{"41b3de4355555557", "333333333.33333343"},
		{"becbf647612f3696", "-0.0000033333333333333333"},
		{"43143ff3c1cb0959", "1424953923781206.2"},
	}
	for _, tt := range tests {
		t.Run(tt.bits, func(t *testing.T) {
			raw, err := hex.DecodeString(tt.bits)
			if err != nil {
				t.Fatal(err)
			}
			var u uint64
			for _, b := range raw {
				u = u<<8 | uint64(b)
			}
			f := math.Float64frombits(u)
			got, err := FormatNumber(f)
			if err != nil {
				t.Fatalf("FormatNumber: %v", err)
			}
			if got != tt.want {
				t.Errorf("FormatNumber = %q, want %q", got, tt.want)
			}
			// The rendering must parse back to the same double.
			back, err := strconv.ParseFloat(got, 64)
			if err != nil {
				t.Fatalf("parse back %q: %v", got, err)
			}
			if back != f {
				t.Errorf("%q does not round-trip to %s", got, tt.bits)
			}
		})
	}
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := FormatNumber(f); err == nil {
			t.Errorf("FormatNumber(%v) accepted", f)
		}
	}
}

// TestTransform covers the worked example of RFC 8785 section 3.2.3 and
// the member ordering example of the same section.
func TestTransform(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "rfc example",
			in: `{
  "numbers": [333333333.33333329, 1E30, 4.50, 2e-3, 0.000000000000000000000000001],
  "string": "\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/",
  "literals": [null, true, false]
}`,
			want: "{\"literals\":[null,true,false],\"numbers\":[333333333.3333333,1e+30,4.5,0.002,1e-27],\"string\":\"\u20ac$\\u000f\\nA'B\\\"\\\\\\\\\\\"/\"}",
		},
		{
			name: "member ordering by utf-16",
			in: `{
  "\u20ac": "Euro Sign",
  "\r": "Carriage Return",
  "\ufb33": "Hebrew Letter Dalet With Dagesh",
  "1": "One",
  "\ud83d\ude00": "Emoji: Grinning Face",
  "\u0080": "Control",
  "\u00f6": "Latin Small Letter O With Diaeresis"
}`,
			want: "{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"\u00f6\":\"Latin Small Letter O With Diaeresis\",\"\u20ac\":\"Euro Sign\",\"\U0001f600\":\"Emoji: Grinning Face\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}",
		},
		{
			name: "nested and empty",
			in:   `{"b":{},"a":[],"c":[{"z":1,"y":[2,3]}]}`,
			want: `{"a":[],"b":{},"c":[{"y":[2,3],"z":1}]}`,
		},
		{
			name: "html characters stay literal",
			in:   `{"h":"<a href=\"x\">&</a>"}`,
			want: `{"h":"<a href=\"x\">&</a>"}`,
		},
		{
			name: "integers and negative zero",
			in:   `[1.0, -0, 100, 1e2, 12345678901234567890]`,
			want: `[1,0,100,100,12345678901234567000]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Transform([]byte(tt.in))
			if err != nil {
				t.Fatalf("Transform: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("Transform =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
	for _, bad := range []string{``, `{`, `{"a":1} x`, `[1e400]`} {
		if _, err := Transform([]byte(bad)); err == nil {
			t.Errorf("Transform(%q) accepted", bad)
		}
	}
}
