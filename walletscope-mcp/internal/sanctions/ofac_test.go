package sanctions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
)

var sample = filepath.Join("..", "..", "testdata", "sanctions", "sdn_sample.xml")

// Real SDN identifiers from the fixture excerpt.
const (
	lazarusETH = "0x098B716B8Aaf21512996dC57EB0615e2383E2f96" // LAZARUS GROUP, Digital Currency Address - ETH
	hanafinUSD = "0x38735f03b30FbC022DdD06ABED01F0Ca823C6a94" // HANAFIN, Digital Currency Address - USDT
)

func loaded(t *testing.T) *OFAC {
	t.Helper()
	o := NewOFAC(OFACOptions{Source: sample, Refresh: time.Hour})
	if err := o.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestParseSample(t *testing.T) {
	f, err := os.Open(sample)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	idx, info, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	// 8 Lazarus ETH + 1 Hanafin USDT + 2 synthetic EVM ids; the XBT id is not indexed.
	if info.Entries != 3 || info.EVMAddresses != 11 || len(idx) != 11 || info.PublishDate != "10/28/2026" {
		t.Fatalf("info=%+v len=%d", info, len(idx))
	}
}

func TestDirectMatchIncludesRecordDetails(t *testing.T) {
	o := loaded(t)
	// Lower-case input normalizes to the same key.
	r := o.Check(context.Background(), blockchain.MustNormalize(strings.ToLower(lazarusETH)))
	if r.Status != StatusMatch || len(r.Matches) != 1 {
		t.Fatalf("%+v", r)
	}
	m := r.Matches[0]
	if m.Name != "LAZARUS GROUP" || m.UID != "27307" || m.ListedAsset != "ETH" || len(m.Programs) == 0 || m.Programs[0] != "DPRK3" || m.ListedValue != lazarusETH {
		t.Fatalf("record: %+v", m)
	}
	if r.Dataset.LastSuccessfulLoad == nil || r.Dataset.SourceURL == "" || r.Disclaimer == "" {
		t.Fatalf("provenance missing: %+v", r.Dataset)
	}
}

func TestEVMAddressListedUnderTokenAsset(t *testing.T) {
	r := loaded(t).Check(context.Background(), hanafinUSD)
	if r.Status != StatusMatch || r.Matches[0].ListedAsset != "USDT" {
		t.Fatalf("%+v", r)
	}
}

func TestNoMatchOnlyWithLoadedDataset(t *testing.T) {
	r := loaded(t).Check(context.Background(), "0x1111111111111111111111111111111111111111")
	if r.Status != StatusNoMatch || len(r.Matches) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestUnavailableSourceIsInconclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	o := NewOFAC(OFACOptions{Source: srv.URL, WaitOnQuery: 10 * time.Millisecond})
	if err := o.Load(context.Background()); err == nil {
		t.Fatal("expected load error")
	}
	r := o.Check(context.Background(), lazarusETH)
	if r.Status != StatusInconclusive || !strings.Contains(r.Reason, "HTTP 503") {
		t.Fatalf("%+v", r)
	}
	if _, ok := o.Lookup(lazarusETH); ok {
		t.Fatal("lookup must report not loaded")
	}
}

func TestFailedRefreshKeepsPreviousDataset(t *testing.T) {
	body, _ := os.ReadFile(sample)
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.Write([]byte("<html>maintenance</html>"))
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	o := NewOFAC(OFACOptions{Source: srv.URL})
	if err := o.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if err := o.Load(context.Background()); err == nil {
		t.Fatal("a page without sdnEntry records must be rejected")
	}
	r := o.Check(context.Background(), lazarusETH)
	if r.Status != StatusMatch || r.Dataset.LastError == "" {
		t.Fatalf("previous dataset must be kept and the error surfaced: %+v", r)
	}
}

func TestMalformedXML(t *testing.T) {
	if _, _, err := Parse(strings.NewReader("<sdnList><sdnEntry><uid>1</uid>")); err == nil {
		t.Fatal("expected error")
	}
}

func TestCheckWaitsForInitialLoad(t *testing.T) {
	body, _ := os.ReadFile(sample)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Write(body)
	}))
	defer srv.Close()
	o := NewOFAC(OFACOptions{Source: srv.URL, Refresh: time.Hour, WaitOnQuery: 5 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.Start(ctx)
	if r := o.Check(context.Background(), lazarusETH); r.Status != StatusMatch {
		t.Fatalf("%+v", r)
	}
}

func TestStaleFlag(t *testing.T) {
	o := loaded(t)
	o.now = func() time.Time { return time.Now().Add(3 * time.Hour) }
	if !o.Info().Stale {
		t.Fatal("dataset older than 2x refresh must be stale")
	}
}

func TestInitialLoadRetriesQuickly(t *testing.T) {
	body, _ := os.ReadFile(sample)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway) // transient failure on first attempt
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	o := NewOFAC(OFACOptions{Source: srv.URL, Refresh: 24 * time.Hour, WaitOnQuery: 10 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.Start(ctx)
	if r := o.Check(context.Background(), lazarusETH); r.Status != StatusMatch {
		t.Fatalf("expected recovery within the query wait: %+v", r)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d", hits.Load())
	}
}
