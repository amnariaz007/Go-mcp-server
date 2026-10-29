package blockchain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/cache"
	"github.com/amnariaz007/walletscope-mcp/internal/observability"
	"github.com/amnariaz007/walletscope-mcp/internal/ratelimit"
)

const maxResponseBytes = 16 << 20

// BlockscoutOptions configures a Blockscout client.
type BlockscoutOptions struct {
	BaseURL        string // e.g. https://eth.blockscout.com/api/v2
	HTTPClient     *http.Client
	RequestTimeout time.Duration
	Limiter        *ratelimit.Limiter
	Cache          *cache.Cache[[]byte]
	MaxRetries     int
	Logger         *slog.Logger
	// Backoff tuning; zero values select defaults.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// Blockscout implements Provider against the Blockscout REST API v2.
type Blockscout struct {
	opt   BlockscoutOptions
	sleep func(context.Context, time.Duration) error
}

// NewBlockscout constructs a client. HTTPClient, Limiter and Cache may be nil.
func NewBlockscout(opt BlockscoutOptions) *Blockscout {
	opt.BaseURL = strings.TrimRight(opt.BaseURL, "/")
	if opt.HTTPClient == nil {
		opt.HTTPClient = &http.Client{}
	}
	if opt.RequestTimeout <= 0 {
		opt.RequestTimeout = 20 * time.Second
	}
	if opt.BaseBackoff <= 0 {
		opt.BaseBackoff = 300 * time.Millisecond
	}
	if opt.MaxBackoff <= 0 {
		opt.MaxBackoff = 8 * time.Second
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Blockscout{opt: opt, sleep: sleepCtx}
}

// Name implements Provider.
func (b *Blockscout) Name() string { return "blockscout" }

// BaseURL implements Provider.
func (b *Blockscout) BaseURL() string { return b.opt.BaseURL }

// ---- wire types -------------------------------------------------------------

type bsTag struct {
	Name    string `json:"name"`
	TagType string `json:"tagType"`
}

type bsAddressRef struct {
	Hash       string  `json:"hash"`
	IsContract *bool   `json:"is_contract"`
	Name       *string `json:"name"`
	ENS        *string `json:"ens_domain_name"`
	Metadata   *struct {
		Tags []bsTag `json:"tags"`
	} `json:"metadata"`
	PublicTags []json.RawMessage `json:"public_tags"`
}

type bsAddress struct {
	bsAddressRef
	CoinBalance        *string `json:"coin_balance"`
	BalanceBlock       *uint64 `json:"block_number_balance_updated_at"`
	IsScam             bool    `json:"is_scam"`
	IsVerified         bool    `json:"is_verified"`
	CreationTxHash     *string `json:"creation_transaction_hash"`
	CreatorAddressHash *string `json:"creator_address_hash"`
	HasTokenTransfers  bool    `json:"has_token_transfers"`
	ProxyType          *string `json:"proxy_type"`
}

type bsCounters struct {
	TransactionsCount   *string `json:"transactions_count"`
	TokenTransfersCount *string `json:"token_transfers_count"`
}

type bsPage[T any] struct {
	Items          []T                        `json:"items"`
	NextPageParams map[string]json.RawMessage `json:"next_page_params"`
}

type bsTx struct {
	Hash            string        `json:"hash"`
	BlockNumber     *uint64       `json:"block_number"`
	Timestamp       *string       `json:"timestamp"`
	From            *bsAddressRef `json:"from"`
	To              *bsAddressRef `json:"to"`
	CreatedContract *bsAddressRef `json:"created_contract"`
	Value           *string       `json:"value"`
	Status          *string       `json:"status"`
	Result          *string       `json:"result"`
	Method          *string       `json:"method"`
}

type bsInternal struct {
	TxHash          string        `json:"transaction_hash"`
	Index           *int          `json:"index"`
	BlockNumber     *uint64       `json:"block_number"`
	Timestamp       *string       `json:"timestamp"`
	From            *bsAddressRef `json:"from"`
	To              *bsAddressRef `json:"to"`
	CreatedContract *bsAddressRef `json:"created_contract"`
	Value           *string       `json:"value"`
	Success         *bool         `json:"success"`
	Type            *string       `json:"type"`
}

type bsToken struct {
	AddressHash string  `json:"address_hash"`
	Address     string  `json:"address"` // older deployments
	Symbol      *string `json:"symbol"`
	Name        *string `json:"name"`
	Decimals    *string `json:"decimals"`
	Type        *string `json:"type"`
	Reputation  *string `json:"reputation"`
}

type bsTokenTransfer struct {
	TxHash      string        `json:"transaction_hash"`
	LogIndex    *int          `json:"log_index"`
	BlockNumber *uint64       `json:"block_number"`
	Timestamp   *string       `json:"timestamp"`
	From        *bsAddressRef `json:"from"`
	To          *bsAddressRef `json:"to"`
	Token       *bsToken      `json:"token"`
	Total       *struct {
		Decimals *string `json:"decimals"`
		Value    *string `json:"value"`
	} `json:"total"`
	Method *string `json:"method"`
}

// ---- Provider implementation ------------------------------------------------

// GetAddress implements Provider.
func (b *Blockscout) GetAddress(ctx context.Context, addr string) (*AddressInfo, error) {
	var w bsAddress
	if err := b.getJSON(ctx, "address", "/addresses/"+addr, nil, &w); err != nil {
		return nil, err
	}
	ref, ok := w.bsAddressRef.normalize()
	if !ok || ref.Address != addr {
		return nil, &UpstreamError{Category: "address", Err: fmt.Errorf("%w: address hash missing or mismatched", ErrBadResponse)}
	}
	info := &AddressInfo{
		Address:           ref.Address,
		Name:              ref.Name,
		ENS:               ref.ENS,
		Labels:            ref.Labels,
		IsScam:            w.IsScam,
		IsVerified:        w.IsVerified,
		BalanceBlock:      w.BalanceBlock,
		HasTokenTransfers: w.HasTokenTransfers,
		ProxyType:         deref(w.ProxyType),
		CreationTxHash:    deref(w.CreationTxHash),
		CreatorAddress:    normalizeLoose(deref(w.CreatorAddressHash)),
	}
	if ref.IsContract != nil {
		info.IsContract = *ref.IsContract
	}
	if w.CoinBalance != nil {
		v, ok := new(big.Int).SetString(*w.CoinBalance, 10)
		if !ok || v.Sign() < 0 {
			return nil, &UpstreamError{Category: "address", Err: fmt.Errorf("%w: invalid coin_balance %q", ErrBadResponse, *w.CoinBalance)}
		}
		info.BalanceWei = v
	}
	return info, nil
}

// GetCounters implements Provider.
func (b *Blockscout) GetCounters(ctx context.Context, addr string) (*Counters, error) {
	var w bsCounters
	if err := b.getJSON(ctx, "counters", "/addresses/"+addr+"/counters", nil, &w); err != nil {
		return nil, err
	}
	c := &Counters{}
	parse := func(s *string) (*int64, error) {
		if s == nil {
			return nil, nil
		}
		n, err := strconv.ParseInt(*s, 10, 64)
		if err != nil || n < 0 {
			return nil, &UpstreamError{Category: "counters", Err: fmt.Errorf("%w: invalid counter %q", ErrBadResponse, *s)}
		}
		return &n, nil
	}
	var err error
	if c.TransactionsCount, err = parse(w.TransactionsCount); err != nil {
		return nil, err
	}
	if c.TokenTransfersCount, err = parse(w.TokenTransfersCount); err != nil {
		return nil, err
	}
	return c, nil
}

// Transactions implements Provider.
func (b *Blockscout) Transactions(ctx context.Context, addr string, f Filter, cursor string) (Page, error) {
	var w bsPage[bsTx]
	q, err := pageQuery(f, cursor)
	if err != nil {
		return Page{}, err
	}
	if err := b.getJSON(ctx, "transactions", "/addresses/"+addr+"/transactions", q, &w); err != nil {
		return Page{}, err
	}
	out := Page{NextCursor: encodeCursor(w.NextPageParams)}
	for _, t := range w.Items {
		a, err := t.normalize()
		if err != nil {
			return Page{}, &UpstreamError{Category: "transactions", Err: err}
		}
		out.Items = append(out.Items, a)
	}
	return out, nil
}

// InternalTransfers implements Provider.
func (b *Blockscout) InternalTransfers(ctx context.Context, addr string, f Filter, cursor string) (Page, error) {
	var w bsPage[bsInternal]
	q, err := pageQuery(f, cursor)
	if err != nil {
		return Page{}, err
	}
	if err := b.getJSON(ctx, "internal_transactions", "/addresses/"+addr+"/internal-transactions", q, &w); err != nil {
		return Page{}, err
	}
	out := Page{NextCursor: encodeCursor(w.NextPageParams)}
	for _, t := range w.Items {
		a, ok, err := t.normalize()
		if err != nil {
			return Page{}, &UpstreamError{Category: "internal_transactions", Err: err}
		}
		if ok {
			out.Items = append(out.Items, a)
		}
	}
	return out, nil
}

// TokenTransfers implements Provider. Only ERC-20 transfers are requested.
func (b *Blockscout) TokenTransfers(ctx context.Context, addr string, f Filter, cursor string) (Page, error) {
	var w bsPage[bsTokenTransfer]
	q, err := pageQuery(f, cursor)
	if err != nil {
		return Page{}, err
	}
	q.Set("type", "ERC-20")
	if err := b.getJSON(ctx, "token_transfers", "/addresses/"+addr+"/token-transfers", q, &w); err != nil {
		return Page{}, err
	}
	out := Page{NextCursor: encodeCursor(w.NextPageParams)}
	for _, t := range w.Items {
		a, err := t.normalize()
		if err != nil {
			return Page{}, &UpstreamError{Category: "token_transfers", Err: err}
		}
		out.Items = append(out.Items, a)
	}
	return out, nil
}

// ---- normalization -----------------------------------------------------------

func (r *bsAddressRef) normalize() (AddressRef, bool) {
	if r == nil {
		return AddressRef{}, false
	}
	addr := normalizeLoose(r.Hash)
	if addr == "" {
		return AddressRef{}, false
	}
	ref := AddressRef{Address: addr, IsContract: r.IsContract, Name: deref(r.Name), ENS: deref(r.ENS)}
	if r.Metadata != nil {
		for _, t := range r.Metadata.Tags {
			if t.Name == "" {
				continue
			}
			label := t.Name
			if t.TagType != "" {
				label += " (" + t.TagType + ")"
			}
			ref.Labels = append(ref.Labels, label)
		}
	}
	for _, raw := range r.PublicTags {
		var pt struct {
			DisplayName string `json:"display_name"`
			Label       string `json:"label"`
		}
		if json.Unmarshal(raw, &pt) == nil {
			if s := firstNonEmpty(pt.DisplayName, pt.Label); s != "" {
				ref.Labels = append(ref.Labels, s+" (public tag)")
			}
		}
	}
	return ref, true
}

func parseTime(s *string) (*time.Time, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid timestamp %q", ErrBadResponse, *s)
	}
	t = t.UTC()
	return &t, nil
}

func parseWei(s *string) (*big.Int, error) {
	if s == nil || *s == "" {
		return new(big.Int), nil
	}
	v, ok := new(big.Int).SetString(*s, 10)
	if !ok || v.Sign() < 0 {
		return nil, fmt.Errorf("%w: invalid amount %q", ErrBadResponse, *s)
	}
	return v, nil
}

func (t bsTx) normalize() (Activity, error) {
	from, ok := t.From.normalize()
	if !ok || t.Hash == "" {
		return Activity{}, fmt.Errorf("%w: transaction missing hash or sender", ErrBadResponse)
	}
	ts, err := parseTime(t.Timestamp)
	if err != nil {
		return Activity{}, err
	}
	v, err := parseWei(t.Value)
	if err != nil {
		return Activity{}, err
	}
	a := Activity{TxHash: strings.ToLower(t.Hash), BlockNumber: t.BlockNumber, Timestamp: ts, From: from, Asset: NativeAsset(), Method: deref(t.Method)}
	if to, ok := t.To.normalize(); ok {
		a.To = &to
	} else if cc, ok := t.CreatedContract.normalize(); ok {
		a.To = &cc
		a.Method = firstNonEmpty(a.Method, "contract_creation")
	}
	switch {
	case t.BlockNumber == nil || t.Status == nil:
		a.Status = StatusPending
	case *t.Status == "ok":
		a.Status = StatusSuccess
	default:
		a.Status = StatusFailed
	}
	a.SetValue(v)
	a.Kind = KindContractCall
	if v.Sign() > 0 {
		a.Kind = KindNativeTransfer
	}
	a.TransferConfirmed = a.Status == StatusSuccess && v.Sign() > 0 && a.To != nil
	return a, nil
}

// normalize returns ok=false for zero-value internal calls, which move no ETH
// and are not transfers.
func (t bsInternal) normalize() (Activity, bool, error) {
	from, ok := t.From.normalize()
	if !ok || t.TxHash == "" {
		return Activity{}, false, fmt.Errorf("%w: internal transaction missing hash or sender", ErrBadResponse)
	}
	v, err := parseWei(t.Value)
	if err != nil {
		return Activity{}, false, err
	}
	if v.Sign() == 0 {
		return Activity{}, false, nil
	}
	ts, err := parseTime(t.Timestamp)
	if err != nil {
		return Activity{}, false, err
	}
	a := Activity{Kind: KindInternalTransfer, TxHash: strings.ToLower(t.TxHash), TraceIndex: t.Index, BlockNumber: t.BlockNumber, Timestamp: ts, From: from, Asset: NativeAsset(), Method: deref(t.Type)}
	if to, ok := t.To.normalize(); ok {
		a.To = &to
	} else if cc, ok := t.CreatedContract.normalize(); ok {
		a.To = &cc
	}
	a.Status = StatusFailed
	if t.Success != nil && *t.Success {
		a.Status = StatusSuccess
	}
	a.SetValue(v)
	a.TransferConfirmed = a.Status == StatusSuccess && a.To != nil
	return a, true, nil
}

func (t bsTokenTransfer) normalize() (Activity, error) {
	from, okFrom := t.From.normalize()
	to, okTo := t.To.normalize()
	if !okFrom || !okTo || t.TxHash == "" || t.Token == nil {
		return Activity{}, fmt.Errorf("%w: token transfer missing hash, token or participants", ErrBadResponse)
	}
	contract := normalizeLoose(firstNonEmpty(t.Token.AddressHash, t.Token.Address))
	if contract == "" {
		return Activity{}, fmt.Errorf("%w: token transfer missing token contract", ErrBadResponse)
	}
	asset := Asset{Type: AssetERC20, Symbol: deref(t.Token.Symbol), Name: deref(t.Token.Name), Contract: contract, Reputation: deref(t.Token.Reputation)}
	decStr := t.Token.Decimals
	if decStr == nil && t.Total != nil {
		decStr = t.Total.Decimals
	}
	if decStr != nil {
		if d, err := strconv.Atoi(*decStr); err == nil && d >= 0 && d <= 255 {
			asset.Decimals = &d
		}
	}
	var raw *string
	if t.Total != nil {
		raw = t.Total.Value
	}
	v, err := parseWei(raw)
	if err != nil {
		return Activity{}, err
	}
	ts, err := parseTime(t.Timestamp)
	if err != nil {
		return Activity{}, err
	}
	a := Activity{Kind: KindTokenTransfer, TxHash: strings.ToLower(t.TxHash), LogIndex: t.LogIndex, BlockNumber: t.BlockNumber, Timestamp: ts, From: from, To: &to, Asset: asset, Method: deref(t.Method), Status: StatusSuccess}
	a.SetValue(v)
	// An ERC-20 Transfer event only exists in the logs of a successful transaction.
	a.TransferConfirmed = v.Sign() > 0
	return a, nil
}

// ---- pagination ---------------------------------------------------------------

func pageQuery(f Filter, cursor string) (url.Values, error) {
	q := url.Values{}
	if cursor != "" {
		parsed, err := url.ParseQuery(cursor)
		if err != nil {
			return nil, fmt.Errorf("invalid pagination cursor: %w", err)
		}
		q = parsed
	}
	if f != FilterAll {
		q.Set("filter", string(f))
	}
	return q, nil
}

// encodeCursor turns next_page_params into a deterministic query string.
func encodeCursor(params map[string]json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	q := url.Values{}
	for _, k := range keys {
		raw := bytes.TrimSpace(params[k])
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			q.Set(k, s)
		} else {
			q.Set(k, string(raw)) // numbers and booleans verbatim
		}
	}
	return q.Encode()
}

// ---- HTTP with retries --------------------------------------------------------

func (b *Blockscout) getJSON(ctx context.Context, category, path string, q url.Values, out any) error {
	u := b.opt.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	stats := observability.Stats(ctx)
	if b.opt.Cache != nil {
		if body, ok := b.opt.Cache.Get(u); ok {
			stats.CacheHits.Add(1)
			return b.decode(category, body, out)
		}
		stats.CacheMisses.Add(1)
	}
	body, err := b.fetch(ctx, category, u)
	if err != nil {
		stats.UpstreamErrors.Add(1)
		return err
	}
	if err := b.decode(category, body, out); err != nil {
		stats.UpstreamErrors.Add(1)
		return err
	}
	if b.opt.Cache != nil {
		b.opt.Cache.Set(u, body) // only well-formed successful responses are cached
	}
	return nil
}

func (b *Blockscout) decode(category string, body []byte, out any) error {
	if err := json.Unmarshal(body, out); err != nil {
		return &UpstreamError{Category: category, Err: fmt.Errorf("%w: %v", ErrBadResponse, err)}
	}
	return nil
}

func (b *Blockscout) fetch(ctx context.Context, category, u string) ([]byte, error) {
	stats := observability.Stats(ctx)
	for attempt := 0; ; attempt++ {
		if b.opt.Limiter != nil {
			if err := b.opt.Limiter.Wait(ctx); err != nil {
				return nil, err
			}
		}
		stats.UpstreamRequests.Add(1)
		start := time.Now()
		body, status, retryAfter, err := b.do(ctx, category, u)
		b.opt.Logger.Debug("upstream request", "provider", "blockscout", "category", category,
			"status", status, "latency_ms", time.Since(start).Milliseconds(), "attempt", attempt+1)
		if err == nil {
			return body, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var ue *UpstreamError
		retryable := errors.As(err, &ue) && (errors.Is(ue.Err, ErrRateLimited) || errors.Is(ue.Err, ErrUnavailable))
		if !retryable || attempt >= b.opt.MaxRetries {
			b.opt.Logger.Warn("upstream request failed", "provider", "blockscout", "category", category, "status", status, "attempts", attempt+1, "error", err.Error())
			return nil, err
		}
		wait := b.backoff(attempt)
		if retryAfter > 0 {
			wait = min(retryAfter, b.opt.MaxBackoff*4)
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < wait {
			return nil, err // waiting would exceed the caller's deadline
		}
		stats.Retries.Add(1)
		if serr := b.sleep(ctx, wait); serr != nil {
			return nil, serr
		}
	}
}

// backoff is exponential with full jitter, capped.
func (b *Blockscout) backoff(attempt int) time.Duration {
	d := b.opt.BaseBackoff << attempt
	if d > b.opt.MaxBackoff || d <= 0 {
		d = b.opt.MaxBackoff
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func (b *Blockscout) do(ctx context.Context, category, u string) (body []byte, status int, retryAfter time.Duration, err error) {
	rctx, cancel := context.WithTimeout(ctx, b.opt.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, 0, &UpstreamError{Category: category, Err: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "walletscope-mcp")
	resp, err := b.opt.HTTPClient.Do(req)
	if err != nil {
		// Transport errors and per-request timeouts are transient.
		return nil, 0, 0, &UpstreamError{Category: category, Err: fmt.Errorf("%w: %v", ErrUnavailable, redactURL(err))}
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	status = resp.StatusCode
	switch {
	case status == http.StatusOK:
		if rerr != nil {
			return nil, status, 0, &UpstreamError{Category: category, Status: status, Err: fmt.Errorf("%w: reading body: %v", ErrUnavailable, rerr)}
		}
		if len(data) > maxResponseBytes {
			return nil, status, 0, &UpstreamError{Category: category, Status: status, Err: fmt.Errorf("%w: response exceeds %d bytes", ErrBadResponse, maxResponseBytes)}
		}
		return data, status, 0, nil
	case status == http.StatusTooManyRequests:
		return nil, status, parseRetryAfter(resp.Header.Get("Retry-After")), &UpstreamError{Category: category, Status: status, Err: ErrRateLimited}
	case status == http.StatusNotFound:
		return nil, status, 0, &UpstreamError{Category: category, Status: status, Err: ErrNotFound}
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return nil, status, 0, &UpstreamError{Category: category, Status: status, Err: ErrInvalidInput}
	case status == http.StatusInternalServerError || status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout:
		return nil, status, parseRetryAfter(resp.Header.Get("Retry-After")), &UpstreamError{Category: category, Status: status, Err: ErrUnavailable}
	default:
		return nil, status, 0, &UpstreamError{Category: category, Status: status, Err: fmt.Errorf("unexpected HTTP status %d", status)}
	}
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// redactURL strips query strings from url.Error messages so that any API keys
// a deployment adds to the base URL never reach logs or tool output.
func redactURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if p, perr := url.Parse(ue.URL); perr == nil {
			p.RawQuery, p.User = "", nil
			return fmt.Errorf("%s %s: %v", ue.Op, p.Redacted(), ue.Err)
		}
	}
	return err
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
