package blockchain

import (
	"errors"
	"math/big"
	"testing"
)

func TestNormalizeAddress(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"0xd8da6bf26964af9d7eed9e03e53415d37aa96045", vitalik, false},
		{"0XD8DA6BF26964AF9D7EED9E03E53415D37AA96045", vitalik, false},
		{vitalik, vitalik, false},
		{"  " + vitalik + " ", vitalik, false},
		{"0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96046", "", true}, // bad checksum
		{"0xd8da6bf26964af9d7eed9e03e53415d37aa9604", "", true},  // too short
		{"d8da6bf26964af9d7eed9e03e53415d37aa960450x", "", true}, // no prefix
		{"0xzz8da6bf26964af9d7eed9e03e53415d37aa9604", "", true}, // non-hex
		{"", "", true},
	}
	for _, c := range cases {
		got, err := NormalizeAddress(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("NormalizeAddress(%q) = %q, %v", c.in, got, err)
		}
		if err != nil && !errors.Is(err, ErrInvalidAddress) {
			t.Errorf("error must wrap ErrInvalidAddress: %v", err)
		}
	}
}

func TestFormatUnits(t *testing.T) {
	cases := []struct {
		v    string
		dec  int
		want string
	}{
		{"0", 18, "0"},
		{"1", 18, "0.000000000000000001"},
		{"1000000000000000000", 18, "1"},
		{"1500000000000000000", 18, "1.5"},
		{"123456789", 6, "123.456789"},
		{"5", 0, "5"},
		{"115792089237316195423570985008687907853269984665640564039457584007913129639935", 18,
			"115792089237316195423570985008687907853269984665640564039457.584007913129639935"},
	}
	for _, c := range cases {
		v, _ := new(big.Int).SetString(c.v, 10)
		if got := FormatUnits(v, c.dec); got != c.want {
			t.Errorf("FormatUnits(%s,%d) = %s, want %s", c.v, c.dec, got, c.want)
		}
	}
}

func TestUnknownDecimalsLeavesFormattedEmpty(t *testing.T) {
	a := NewAmount(big.NewInt(42), -1, "XYZ")
	if a.Decimals != nil || a.Formatted != "" || a.Raw != "42" {
		t.Fatalf("%+v", a)
	}
}

func TestDirection(t *testing.T) {
	me, other := "0x1111111111111111111111111111111111111111", "0x2222222222222222222222222222222222222222"
	mk := func(from, to string) Activity {
		return Activity{From: AddressRef{Address: from}, To: &AddressRef{Address: to}}
	}
	for _, c := range []struct {
		a    Activity
		want string
	}{
		{mk(me, other), DirOut}, {mk(other, me), DirIn}, {mk(me, me), DirSelf}, {mk(other, other), ""},
	} {
		if got := c.a.DirectionFor(me); got != c.want {
			t.Errorf("direction = %q want %q", got, c.want)
		}
	}
}
