package main

import "testing"

func TestParseTokenAmount(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"500000", 500000, true},
		{"500k", 500_000, true},
		{"2m", 2_000_000, true},
		{"2.5m", 2_500_000, true},
		{"1.5K", 1_500, true},
		{" 10k ", 10_000, true},
		{"0", 0, false},
		{"-5k", 0, false},
		{"tokens", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, err := parseTokenAmount(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("parseTokenAmount(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("parseTokenAmount(%q) accepted, want error", c.in)
		}
	}
}

func TestHumanTokens(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1234, "1234"},
		{10_000, "10k"},
		{500_000, "500k"},
		{1_500_000, "1.5m"},
		{2_000_000, "2m"},
		{-500_000, "-500k"},
	}
	for _, c := range cases {
		if got := humanTokens(c.in); got != c.want {
			t.Errorf("humanTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
