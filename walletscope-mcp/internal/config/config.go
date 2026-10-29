// Package config loads and validates WalletScope runtime configuration from
// environment variables. Every setting has a documented default; invalid values
// fail fast at startup with a descriptive error.
package config

import (
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Transport modes.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// DefaultOFACSource is the official OFAC Sanctions List Service SDN XML export.
const DefaultOFACSource = "https://sanctionslistservice.ofac.treas.gov/api/PublicationPreview/exports/SDN.XML"

// DefaultBlockscoutURL is the public Blockscout instance for Ethereum mainnet.
const DefaultBlockscoutURL = "https://eth.blockscout.com/api/v2"

// DefaultMixers are Tornado Cash Ethereum mainnet contracts. Each address was
// verified against its Blockscout verified-contract name (see docs/risk-model.md).
var DefaultMixers = []Mixer{
	{Address: "0x12D66f87A04A9E220743712cE6d9bB1B5616B8Fc", Label: "Tornado Cash 0.1 ETH pool"},
	{Address: "0x47CE0C6eD5B0Ce3d3A51fdb1C52DC66a7c3c2936", Label: "Tornado Cash 1 ETH pool"},
	{Address: "0x910Cbd523D972eb0a6f4cAe4618aD62622b39DbF", Label: "Tornado Cash 10 ETH pool"},
	{Address: "0xA160cdAB225685dA1d56aa342Ad8841c3b53f291", Label: "Tornado Cash 100 ETH pool"},
	{Address: "0xd90e2f925DA726b50C4Ed8D0Fb90Ad053324F31b", Label: "Tornado Cash Router"},
	{Address: "0x722122dF12D4e14e13Ac3b6895a86e84145b6967", Label: "Tornado Cash Proxy"},
}

// Mixer is a configured mixer contract.
type Mixer struct {
	Address string
	Label   string
}

// Config is the complete validated runtime configuration.
type Config struct {
	Transport string

	HTTPAddr                 string
	HTTPAuthToken            string
	HTTPAllowUnauthenticated bool

	BlockscoutURL   string
	UpstreamTimeout time.Duration
	UpstreamRPS     float64
	UpstreamBurst   int
	UpstreamRetries int

	CacheTTL        time.Duration
	CacheMaxEntries int

	TraceWorkers         int
	TraceMaxHops         int
	TraceMaxNodes        int
	TraceMaxEdges        int
	TraceMaxAPICalls     int
	TraceMaxChildren     int
	TraceTimeout         time.Duration
	TraceExpandContracts bool

	OFACSource      string
	OFACRefresh     time.Duration
	OFACLoadTimeout time.Duration
	OFACWaitOnQuery time.Duration

	Mixers []Mixer

	RiskHistoryPages       int
	RiskTimeout            time.Duration
	StructuringMinDeposits int
	StructuringWindow      time.Duration
	StructuringMultiple    int
	StructuringCoveragePct int
	NewWalletAge           time.Duration
	LargeTransferWei       *big.Int
	LargeTransferETH       string
	ToolTimeout            time.Duration
	LogLevel               string
	LogFormat              string
}

// Load reads configuration from the process environment.
func Load() (*Config, error) { return LoadFrom(os.LookupEnv) }

// LoadFrom reads configuration using the supplied lookup function, which makes
// the loader testable without mutating the process environment.
func LoadFrom(lookup func(string) (string, bool)) (*Config, error) {
	p := &parser{lookup: lookup}
	c := &Config{
		Transport:                p.str("WALLETSCOPE_TRANSPORT", TransportStdio),
		HTTPAddr:                 p.str("WALLETSCOPE_HTTP_ADDR", "127.0.0.1:8080"),
		HTTPAuthToken:            p.str("WALLETSCOPE_HTTP_AUTH_TOKEN", ""),
		HTTPAllowUnauthenticated: p.boolean("WALLETSCOPE_HTTP_ALLOW_UNAUTHENTICATED", false),

		BlockscoutURL:   strings.TrimRight(p.str("WALLETSCOPE_BLOCKSCOUT_URL", DefaultBlockscoutURL), "/"),
		UpstreamTimeout: p.duration("WALLETSCOPE_UPSTREAM_TIMEOUT", 20*time.Second),
		UpstreamRPS:     p.float("WALLETSCOPE_UPSTREAM_RPS", 5),
		UpstreamBurst:   p.integer("WALLETSCOPE_UPSTREAM_BURST", 10),
		UpstreamRetries: p.integer("WALLETSCOPE_UPSTREAM_RETRIES", 3),

		CacheTTL:        p.duration("WALLETSCOPE_CACHE_TTL", 2*time.Minute),
		CacheMaxEntries: p.integer("WALLETSCOPE_CACHE_MAX_ENTRIES", 2000),

		TraceWorkers:         p.integer("WALLETSCOPE_TRACE_WORKERS", 4),
		TraceMaxHops:         p.integer("WALLETSCOPE_TRACE_MAX_HOPS", 3),
		TraceMaxNodes:        p.integer("WALLETSCOPE_TRACE_MAX_NODES", 200),
		TraceMaxEdges:        p.integer("WALLETSCOPE_TRACE_MAX_EDGES", 500),
		TraceMaxAPICalls:     p.integer("WALLETSCOPE_TRACE_MAX_API_CALLS", 120),
		TraceMaxChildren:     p.integer("WALLETSCOPE_TRACE_MAX_CHILDREN", 10),
		TraceTimeout:         p.duration("WALLETSCOPE_TRACE_TIMEOUT", 60*time.Second),
		TraceExpandContracts: p.boolean("WALLETSCOPE_TRACE_EXPAND_CONTRACTS", false),

		OFACSource:      p.str("WALLETSCOPE_OFAC_SOURCE", DefaultOFACSource),
		OFACRefresh:     p.duration("WALLETSCOPE_OFAC_REFRESH", 24*time.Hour),
		OFACLoadTimeout: p.duration("WALLETSCOPE_OFAC_LOAD_TIMEOUT", 3*time.Minute),
		OFACWaitOnQuery: p.duration("WALLETSCOPE_OFAC_WAIT_ON_QUERY", 45*time.Second),

		RiskHistoryPages:       p.integer("WALLETSCOPE_RISK_HISTORY_PAGES", 4),
		RiskTimeout:            p.duration("WALLETSCOPE_RISK_TIMEOUT", 90*time.Second),
		StructuringMinDeposits: p.integer("WALLETSCOPE_STRUCTURING_MIN_DEPOSITS", 5),
		StructuringWindow:      p.duration("WALLETSCOPE_STRUCTURING_WINDOW", 72*time.Hour),
		StructuringMultiple:    p.integer("WALLETSCOPE_STRUCTURING_MULTIPLE", 3),
		StructuringCoveragePct: p.integer("WALLETSCOPE_STRUCTURING_COVERAGE_PCT", 50),
		NewWalletAge:           p.duration("WALLETSCOPE_NEW_WALLET_AGE", 30*24*time.Hour),
		LargeTransferETH:       p.str("WALLETSCOPE_LARGE_TRANSFER_ETH", "10"),
		ToolTimeout:            p.duration("WALLETSCOPE_TOOL_TIMEOUT", 45*time.Second),

		LogLevel:  strings.ToLower(p.str("WALLETSCOPE_LOG_LEVEL", "info")),
		LogFormat: strings.ToLower(p.str("WALLETSCOPE_LOG_FORMAT", "json")),
	}
	c.Mixers = p.mixers("WALLETSCOPE_MIXER_ADDRESSES", DefaultMixers)
	// Report parse and semantic errors together so operators can fix everything at once.
	if err := errors.Join(append(p.errs, c.Validate())...); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate checks semantic constraints between fields.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	switch c.Transport {
	case TransportStdio, TransportHTTP:
	default:
		bad("WALLETSCOPE_TRANSPORT must be %q or %q, got %q", TransportStdio, TransportHTTP, c.Transport)
	}
	host, _, err := net.SplitHostPort(c.HTTPAddr)
	if err != nil {
		bad("WALLETSCOPE_HTTP_ADDR %q is not host:port: %v", c.HTTPAddr, err)
	}
	if c.Transport == TransportHTTP && c.HTTPAuthToken == "" && !c.HTTPAllowUnauthenticated && !isLoopback(host) {
		bad("WALLETSCOPE_HTTP_ADDR %q is not a loopback address: set WALLETSCOPE_HTTP_AUTH_TOKEN, or set WALLETSCOPE_HTTP_ALLOW_UNAUTHENTICATED=true only inside a trusted network", c.HTTPAddr)
	}
	if c.HTTPAuthToken != "" && len(c.HTTPAuthToken) < 16 {
		bad("WALLETSCOPE_HTTP_AUTH_TOKEN must be at least 16 characters")
	}
	if u, err := url.Parse(c.BlockscoutURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		bad("WALLETSCOPE_BLOCKSCOUT_URL %q must be an absolute http(s) URL", c.BlockscoutURL)
	}
	positiveDur := map[string]time.Duration{
		"WALLETSCOPE_UPSTREAM_TIMEOUT":   c.UpstreamTimeout,
		"WALLETSCOPE_CACHE_TTL":          c.CacheTTL,
		"WALLETSCOPE_TRACE_TIMEOUT":      c.TraceTimeout,
		"WALLETSCOPE_OFAC_REFRESH":       c.OFACRefresh,
		"WALLETSCOPE_OFAC_LOAD_TIMEOUT":  c.OFACLoadTimeout,
		"WALLETSCOPE_RISK_TIMEOUT":       c.RiskTimeout,
		"WALLETSCOPE_STRUCTURING_WINDOW": c.StructuringWindow,
		"WALLETSCOPE_NEW_WALLET_AGE":     c.NewWalletAge,
		"WALLETSCOPE_TOOL_TIMEOUT":       c.ToolTimeout,
	}
	for name, d := range positiveDur {
		if d <= 0 {
			bad("%s must be positive", name)
		}
	}
	if c.OFACWaitOnQuery < 0 {
		bad("WALLETSCOPE_OFAC_WAIT_ON_QUERY must not be negative")
	}
	inRange := func(name string, v, lo, hi int) {
		if v < lo || v > hi {
			bad("%s must be between %d and %d, got %d", name, lo, hi, v)
		}
	}
	inRange("WALLETSCOPE_UPSTREAM_BURST", c.UpstreamBurst, 1, 1000)
	inRange("WALLETSCOPE_UPSTREAM_RETRIES", c.UpstreamRetries, 0, 10)
	inRange("WALLETSCOPE_CACHE_MAX_ENTRIES", c.CacheMaxEntries, 1, 1_000_000)
	inRange("WALLETSCOPE_TRACE_WORKERS", c.TraceWorkers, 1, 64)
	inRange("WALLETSCOPE_TRACE_MAX_HOPS", c.TraceMaxHops, 1, 3)
	inRange("WALLETSCOPE_TRACE_MAX_NODES", c.TraceMaxNodes, 2, 10_000)
	inRange("WALLETSCOPE_TRACE_MAX_EDGES", c.TraceMaxEdges, 1, 50_000)
	inRange("WALLETSCOPE_TRACE_MAX_API_CALLS", c.TraceMaxAPICalls, 1, 10_000)
	inRange("WALLETSCOPE_TRACE_MAX_CHILDREN", c.TraceMaxChildren, 1, 1000)
	inRange("WALLETSCOPE_RISK_HISTORY_PAGES", c.RiskHistoryPages, 1, 50)
	inRange("WALLETSCOPE_STRUCTURING_MIN_DEPOSITS", c.StructuringMinDeposits, 2, 1000)
	inRange("WALLETSCOPE_STRUCTURING_MULTIPLE", c.StructuringMultiple, 1, 1000)
	inRange("WALLETSCOPE_STRUCTURING_COVERAGE_PCT", c.StructuringCoveragePct, 1, 100)
	if c.UpstreamRPS <= 0 || c.UpstreamRPS > 1000 {
		bad("WALLETSCOPE_UPSTREAM_RPS must be in (0, 1000], got %v", c.UpstreamRPS)
	}
	wei, err := ParseDecimalUnits(c.LargeTransferETH, 18)
	if err != nil || wei.Sign() <= 0 {
		bad("WALLETSCOPE_LARGE_TRANSFER_ETH must be a positive decimal ETH amount, got %q", c.LargeTransferETH)
	}
	c.LargeTransferWei = wei
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		bad("WALLETSCOPE_LOG_LEVEL must be debug, info, warn or error, got %q", c.LogLevel)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		bad("WALLETSCOPE_LOG_FORMAT must be json or text, got %q", c.LogFormat)
	}
	if c.OFACSource == "" {
		bad("WALLETSCOPE_OFAC_SOURCE must not be empty")
	}
	return errors.Join(errs...)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ParseDecimalUnits converts a non-negative decimal string such as "0.1" into
// integer base units with the given number of decimals, without floating point.
func ParseDecimalUnits(s string, decimals int) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty amount")
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if hasDot && frac == "" || whole == "" && frac == "" {
		return nil, fmt.Errorf("invalid amount %q", s)
	}
	if whole == "" {
		whole = "0"
	}
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return nil, fmt.Errorf("invalid amount %q: only digits and one '.' allowed", s)
		}
	}
	if len(frac) > decimals {
		return nil, fmt.Errorf("amount %q has more than %d decimal places", s, decimals)
	}
	frac += strings.Repeat("0", decimals-len(frac))
	v, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok {
		return nil, fmt.Errorf("invalid amount %q", s)
	}
	return v, nil
}

type parser struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (p *parser) raw(key string) (string, bool) {
	v, ok := p.lookup(key)
	if !ok || strings.TrimSpace(v) == "" {
		return "", false
	}
	return strings.TrimSpace(v), true
}

func (p *parser) str(key, def string) string {
	if v, ok := p.raw(key); ok {
		return v
	}
	return def
}

func (p *parser) integer(key string, def int) int {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: invalid integer %q", key, v))
		return def
	}
	return n
}

func (p *parser) float(key string, def float64) float64 {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: invalid number %q", key, v))
		return def
	}
	return f
}

func (p *parser) boolean(key string, def bool) bool {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: invalid boolean %q", key, v))
		return def
	}
	return b
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: invalid duration %q (use Go syntax such as 30s, 5m, 24h)", key, v))
		return def
	}
	return d
}

// mixers parses "0xaddr=Label,0xaddr2=Label2". The value "none" disables the
// mixer rule's address list entirely.
func (p *parser) mixers(key string, def []Mixer) []Mixer {
	v, ok := p.raw(key)
	if !ok {
		return append([]Mixer(nil), def...)
	}
	if strings.EqualFold(v, "none") {
		return nil
	}
	var out []Mixer
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		addr, label, _ := strings.Cut(part, "=")
		addr = strings.TrimSpace(addr)
		if !isHexAddress(addr) {
			p.errs = append(p.errs, fmt.Errorf("%s: %q is not a 0x-prefixed 40-hex-digit address", key, addr))
			continue
		}
		if label = strings.TrimSpace(label); label == "" {
			label = "configured mixer"
		}
		out = append(out, Mixer{Address: addr, Label: label})
	}
	return out
}

func isHexAddress(s string) bool {
	if len(s) != 42 || !strings.HasPrefix(strings.ToLower(s), "0x") {
		return false
	}
	for _, r := range s[2:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}
