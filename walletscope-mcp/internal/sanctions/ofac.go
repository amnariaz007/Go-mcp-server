package sanctions

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
)

const (
	maxDatasetBytes   = 256 << 20
	digitalCurrencyID = "Digital Currency Address - "
	failedRetryDelay  = 5 * time.Minute
	initialRetryBase  = 2 * time.Second
)

// OFACOptions configures the OFAC SDN loader.
type OFACOptions struct {
	Source      string // https URL or local file path of SDN.XML
	HTTPClient  *http.Client
	Refresh     time.Duration
	LoadTimeout time.Duration
	WaitOnQuery time.Duration // how long Check waits for the initial load
	Logger      *slog.Logger
}

// OFAC is an in-memory index of EVM-format digital currency addresses in the
// OFAC SDN list. It is safe for concurrent use.
type OFAC struct {
	opt    OFACOptions
	mu     sync.RWMutex
	index  map[string][]Record
	info   DatasetInfo
	loaded chan struct{} // closed after the first successful load
	once   sync.Once
	now    func() time.Time
}

// NewOFAC creates an unloaded screener. Call Load or Start.
func NewOFAC(opt OFACOptions) *OFAC {
	if opt.HTTPClient == nil {
		opt.HTTPClient = &http.Client{}
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &OFAC{
		opt:    opt,
		loaded: make(chan struct{}),
		info:   DatasetInfo{Name: "OFAC Specially Designated Nationals (SDN) list", SourceURL: opt.Source},
		now:    time.Now,
	}
}

// Start loads the dataset in the background and refreshes it periodically
// until ctx is done. Failed loads keep the previous dataset and are retried.
func (o *OFAC) Start(ctx context.Context) {
	go func() {
		failures := 0
		for {
			delay := o.opt.Refresh
			if err := o.Load(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				failures++
				o.opt.Logger.Error("sanctions dataset load failed", "source", "ofac_sdn", "attempt", failures, "error", err.Error())
				delay = min(delay, failedRetryDelay)
				if !o.Ready() {
					// No usable dataset yet: retry quickly (2s, 4s, 8s ... capped) so a
					// transient network error does not leave screening inconclusive.
					delay = min(delay, initialRetryBase<<min(failures-1, 4))
				}
			} else {
				failures = 0
			}
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}()
}

// Load fetches and parses the dataset, atomically replacing the index on success.
func (o *OFAC) Load(ctx context.Context) error {
	if o.opt.LoadTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.opt.LoadTimeout)
		defer cancel()
	}
	started := o.now().UTC()
	idx, info, err := o.fetchAndParse(ctx)

	o.mu.Lock()
	defer o.mu.Unlock()
	o.info.LastAttempt = &started
	if err != nil {
		o.info.LastError = err.Error()
		return err
	}
	o.index = idx
	o.info.PublishDate, o.info.Entries, o.info.EVMAddresses = info.PublishDate, info.Entries, info.EVMAddresses
	o.info.LastSuccessfulLoad = &started
	o.info.LastError = ""
	o.once.Do(func() { close(o.loaded) })
	o.opt.Logger.Info("sanctions dataset loaded", "source", "ofac_sdn", "publish_date", info.PublishDate,
		"entries", info.Entries, "evm_addresses", info.EVMAddresses, "duration_ms", time.Since(started).Milliseconds())
	return nil
}

func (o *OFAC) fetchAndParse(ctx context.Context) (map[string][]Record, DatasetInfo, error) {
	r, err := o.open(ctx)
	if err != nil {
		return nil, DatasetInfo{}, err
	}
	defer r.Close()
	return Parse(io.LimitReader(r, maxDatasetBytes))
}

func (o *OFAC) open(ctx context.Context) (io.ReadCloser, error) {
	src := o.opt.Source
	if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
		f, err := os.Open(strings.TrimPrefix(src, "file://"))
		if err != nil {
			return nil, fmt.Errorf("open sanctions file: %w", err)
		}
		return f, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "walletscope-mcp")
	resp, err := o.opt.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download sanctions dataset: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download sanctions dataset: HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// xml wire types; tags match local names regardless of the file's namespace.
type sdnEntry struct {
	UID       string   `xml:"uid"`
	FirstName string   `xml:"firstName"`
	LastName  string   `xml:"lastName"`
	SDNType   string   `xml:"sdnType"`
	Programs  []string `xml:"programList>program"`
	IDs       []struct {
		UID      string `xml:"uid"`
		IDType   string `xml:"idType"`
		IDNumber string `xml:"idNumber"`
	} `xml:"idList>id"`
	AKAs []struct {
		FirstName string `xml:"firstName"`
		LastName  string `xml:"lastName"`
	} `xml:"akaList>aka"`
}

// Parse streams an SDN XML document and indexes every digital currency
// identifier with an EVM (0x + 40 hex) shape. A document with no entries or no
// EVM identifiers is rejected as an unexpected format so a broken download can
// never replace a good dataset with an empty one.
func Parse(r io.Reader) (map[string][]Record, DatasetInfo, error) {
	dec := xml.NewDecoder(r)
	idx := map[string][]Record{}
	var info DatasetInfo
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, info, fmt.Errorf("parse sanctions XML: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "Publish_Date":
			var s string
			if err := dec.DecodeElement(&s, &se); err != nil {
				return nil, info, fmt.Errorf("parse Publish_Date: %w", err)
			}
			info.PublishDate = strings.TrimSpace(s)
		case "sdnEntry":
			var e sdnEntry
			if err := dec.DecodeElement(&e, &se); err != nil {
				return nil, info, fmt.Errorf("parse sdnEntry: %w", err)
			}
			info.Entries++
			indexEntry(idx, &e, &info)
		}
	}
	if info.Entries == 0 {
		return nil, info, errors.New("sanctions dataset contained no sdnEntry records (unexpected format)")
	}
	if info.EVMAddresses == 0 {
		return nil, info, errors.New("sanctions dataset contained no EVM digital currency addresses (unexpected format)")
	}
	return idx, info, nil
}

func indexEntry(idx map[string][]Record, e *sdnEntry, info *DatasetInfo) {
	var rec *Record
	for _, id := range e.IDs {
		if !strings.HasPrefix(id.IDType, digitalCurrencyID) {
			continue
		}
		value := strings.TrimSpace(id.IDNumber)
		if !blockchain.IsEVMAddress(value) {
			continue
		}
		// OFAC does not consistently checksum; compare case-insensitively.
		key, err := blockchain.NormalizeAddress(strings.ToLower(value))
		if err != nil {
			continue
		}
		if rec == nil {
			rec = &Record{UID: e.UID, Name: strings.TrimSpace(strings.TrimSpace(e.FirstName) + " " + e.LastName), Type: e.SDNType, Programs: e.Programs}
			for i, a := range e.AKAs {
				if i == 5 {
					break
				}
				rec.Aliases = append(rec.Aliases, strings.TrimSpace(strings.TrimSpace(a.FirstName)+" "+a.LastName))
			}
		}
		r := *rec
		r.IDUID, r.IDType, r.ListedValue = id.UID, id.IDType, value
		r.ListedAsset = strings.TrimSpace(strings.TrimPrefix(id.IDType, digitalCurrencyID))
		idx[key] = append(idx[key], r)
		info.EVMAddresses++
	}
}

// Lookup implements Screener.
func (o *OFAC) Lookup(addr string) ([]Record, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.index == nil {
		return nil, false
	}
	return o.index[addr], true
}

// Info implements Screener.
func (o *OFAC) Info() DatasetInfo {
	o.mu.RLock()
	defer o.mu.RUnlock()
	info := o.info
	if info.LastSuccessfulLoad != nil && o.opt.Refresh > 0 && o.now().Sub(*info.LastSuccessfulLoad) > 2*o.opt.Refresh {
		info.Stale = true
	}
	return info
}

// Ready reports whether a dataset has been loaded.
func (o *OFAC) Ready() bool {
	select {
	case <-o.loaded:
		return true
	default:
		return false
	}
}

// Check implements Screener.
func (o *OFAC) Check(ctx context.Context, addr string) Result {
	if !o.Ready() && o.opt.WaitOnQuery > 0 {
		wctx, cancel := context.WithTimeout(ctx, o.opt.WaitOnQuery)
		select {
		case <-o.loaded:
		case <-wctx.Done():
		}
		cancel()
	}
	res := Result{Address: addr, Matches: []Record{}, CheckedAt: o.now().UTC(), Disclaimer: Disclaimer}
	records, loaded := o.Lookup(addr)
	res.Dataset = o.Info()
	switch {
	case !loaded:
		res.Status = StatusInconclusive
		res.Reason = "The OFAC dataset has not been loaded"
		if res.Dataset.LastError != "" {
			res.Reason += " (last error: " + res.Dataset.LastError + ")"
		} else {
			res.Reason += " (initial download still in progress)"
		}
		res.Reason += "; no conclusion can be drawn."
	case len(records) > 0:
		res.Status = StatusMatch
		res.Matches = records
	default:
		res.Status = StatusNoMatch
	}
	return res
}
