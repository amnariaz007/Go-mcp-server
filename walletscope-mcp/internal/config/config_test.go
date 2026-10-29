package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDefaultsAreValid(t *testing.T) {
	c, err := LoadFrom(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Transport != TransportStdio || c.TraceMaxHops != 3 || len(c.Mixers) != 6 || c.LargeTransferWei.String() != "10000000000000000000" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestInvalidValuesFailFast(t *testing.T) {
	_, err := LoadFrom(env(map[string]string{
		"WALLETSCOPE_TRANSPORT":          "carrier-pigeon",
		"WALLETSCOPE_TRACE_MAX_HOPS":     "7",
		"WALLETSCOPE_CACHE_TTL":          "soon",
		"WALLETSCOPE_UPSTREAM_RPS":       "-1",
		"WALLETSCOPE_BLOCKSCOUT_URL":     "ftp://x",
		"WALLETSCOPE_MIXER_ADDRESSES":    "0x123=bad",
		"WALLETSCOPE_LARGE_TRANSFER_ETH": "1e3",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"WALLETSCOPE_TRANSPORT", "WALLETSCOPE_TRACE_MAX_HOPS", "WALLETSCOPE_CACHE_TTL",
		"WALLETSCOPE_UPSTREAM_RPS", "WALLETSCOPE_BLOCKSCOUT_URL", "WALLETSCOPE_MIXER_ADDRESSES", "WALLETSCOPE_LARGE_TRANSFER_ETH"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

func TestHTTPPublicBindRequiresToken(t *testing.T) {
	base := map[string]string{"WALLETSCOPE_TRANSPORT": "http", "WALLETSCOPE_HTTP_ADDR": "0.0.0.0:8080"}
	if _, err := LoadFrom(env(base)); err == nil {
		t.Fatal("public bind without token must be rejected")
	}
	base["WALLETSCOPE_HTTP_AUTH_TOKEN"] = "short"
	if _, err := LoadFrom(env(base)); err == nil {
		t.Fatal("short token must be rejected")
	}
	base["WALLETSCOPE_HTTP_AUTH_TOKEN"] = "0123456789abcdef0123"
	if _, err := LoadFrom(env(base)); err != nil {
		t.Fatal(err)
	}
	delete(base, "WALLETSCOPE_HTTP_AUTH_TOKEN")
	base["WALLETSCOPE_HTTP_ALLOW_UNAUTHENTICATED"] = "true"
	if _, err := LoadFrom(env(base)); err != nil {
		t.Fatal(err)
	}
}

func TestMixerOverride(t *testing.T) {
	c, err := LoadFrom(env(map[string]string{"WALLETSCOPE_MIXER_ADDRESSES": "0x1111111111111111111111111111111111111111=Test mixer, 0x2222222222222222222222222222222222222222"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Mixers) != 2 || c.Mixers[0].Label != "Test mixer" || c.Mixers[1].Label != "configured mixer" {
		t.Fatalf("%+v", c.Mixers)
	}
	c, _ = LoadFrom(env(map[string]string{"WALLETSCOPE_MIXER_ADDRESSES": "none"}))
	if len(c.Mixers) != 0 {
		t.Fatal("none must disable the list")
	}
}

func TestParseDecimalUnits(t *testing.T) {
	ok := map[string]string{"0.1": "100000000000000000", "1": "1000000000000000000", ".5": "500000000000000000", "0": "0", "0.000000000000000001": "1"}
	for in, want := range ok {
		v, err := ParseDecimalUnits(in, 18)
		if err != nil || v.String() != want {
			t.Errorf("%s -> %v, %v", in, v, err)
		}
	}
	for _, bad := range []string{"", "-1", "1.", "1e18", "0x10", "1.2.3", "0.0000000000000000001", "abc"} {
		if _, err := ParseDecimalUnits(bad, 18); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}
