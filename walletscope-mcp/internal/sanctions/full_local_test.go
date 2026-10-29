package sanctions

import (
	"os"
	"testing"
)

// TestParseFullOfficialFile parses a locally downloaded official SDN.XML when
// WALLETSCOPE_SDN_FILE is set. Skipped in the default offline suite.
func TestParseFullOfficialFile(t *testing.T) {
	p := os.Getenv("WALLETSCOPE_SDN_FILE")
	if p == "" {
		t.Skip("set WALLETSCOPE_SDN_FILE to the official SDN.XML to run")
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	idx, info, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("publish=%s entries=%d evm_ids=%d unique_addresses=%d", info.PublishDate, info.Entries, info.EVMAddresses, len(idx))
	if _, ok := idx["0x098B716B8Aaf21512996dC57EB0615e2383E2f96"]; !ok {
		t.Fatal("Lazarus Group ETH address missing from full dataset")
	}
}
