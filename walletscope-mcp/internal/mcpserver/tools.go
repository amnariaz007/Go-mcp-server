package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
	"github.com/amnariaz007/walletscope-mcp/internal/investigation"
	"github.com/amnariaz007/walletscope-mcp/internal/risk"
	"github.com/amnariaz007/walletscope-mcp/internal/sanctions"
)

const addressPattern = "^0[xX][0-9a-fA-F]{40}$"

// AddressInfoInput is the get_address_info input.
type AddressInfoInput struct {
	Address         string `json:"address" jsonschema:"Ethereum address (0x + 40 hex). Mixed case must be a valid EIP-55 checksum."`
	Chain           string `json:"chain,omitempty" jsonschema:"chain identifier; defaults to ethereum (the only supported chain)"`
	InvestigationID string `json:"investigation_id,omitempty" jsonschema:"optional caller-supplied id to correlate related calls in audit logs"`
}

// TransactionsInput is the get_transactions input.
type TransactionsInput struct {
	Address         string `json:"address" jsonschema:"Ethereum address to list activity for"`
	Limit           int    `json:"limit,omitempty" jsonschema:"maximum items to return (1-100, default 20)"`
	Direction       string `json:"direction,omitempty" jsonschema:"in (received), out (sent) or both (default)"`
	InvestigationID string `json:"investigation_id,omitempty" jsonschema:"optional correlation id"`
}

// TraceInput is the trace_funds input.
type TraceInput struct {
	Address         string `json:"address" jsonschema:"start address"`
	Hops            int    `json:"hops,omitempty" jsonschema:"number of outgoing hops to follow (1-3, default 2)"`
	MinAmount       string `json:"min_amount,omitempty" jsonschema:"per-transfer minimum as a decimal string in whole units of the traced asset (ETH by default), default 0.1"`
	Asset           string `json:"asset,omitempty" jsonschema:"native (ETH, default) or an ERC-20 token contract address; min_amount is in that token's whole units"`
	InvestigationID string `json:"investigation_id,omitempty" jsonschema:"optional correlation id"`
}

// AddressInput is the input of check_sanctions and risk_report.
type AddressInput struct {
	Address         string `json:"address" jsonschema:"Ethereum address"`
	InvestigationID string `json:"investigation_id,omitempty" jsonschema:"optional correlation id"`
}

const (
	defaultTraceHops = 2
	defaultTraceMin  = "0.1"
)

func registerTools(s *mcp.Server, d *Deps) {
	addTool(s, d, &mcp.Tool{
		Name:  "get_address_info",
		Title: "Get address profile",
		Description: "Profile an Ethereum mainnet address: ETH balance, account type (EOA, EIP-7702-delegated EOA or contract), " +
			"provider activity counters, first/last observed activity, ENS and provider-supplied public labels. " +
			"first_observed is returned only when the full indexed history fits the read budget; otherwise it is omitted " +
			"and a limitation explains why. Missing fields mean unknown, never zero.",
		InputSchema: schemaFor[AddressInfoInput](func(s *jsonschema.Schema) {
			s.Properties["address"].Pattern = addressPattern
			s.Properties["chain"].Enum = []any{"ethereum"}
			s.Properties["chain"].Default = []byte(`"ethereum"`)
		}),
		Annotations: readOnly("Get address profile"),
	}, 0, func(in AddressInfoInput) (string, string) { return in.Address, in.InvestigationID },
		func(ctx context.Context, in AddressInfoInput) (investigation.AddressInfoResult, error) {
			if _, err := investigation.ValidateChain(in.Chain); err != nil {
				return investigation.AddressInfoResult{}, err
			}
			addr, err := blockchain.NormalizeAddress(in.Address)
			if err != nil {
				return investigation.AddressInfoResult{}, err
			}
			return deref(d.Investigation.GetAddressInfo(ctx, addr))
		})

	addTool(s, d, &mcp.Tool{
		Name:  "get_transactions",
		Title: "List recent wallet activity",
		Description: "List recent activity for an Ethereum address, newest first, merging top-level transactions, internal ETH " +
			"transfers and ERC-20 transfer events. Each item has kind (native_transfer, contract_call, internal_transfer, " +
			"token_transfer), direction relative to the wallet, exact amount in base units plus formatted value, status, and " +
			"transfer_confirmed (false for failed/pending/zero-value items, which never count as moved funds).",
		InputSchema: schemaFor[TransactionsInput](func(s *jsonschema.Schema) {
			s.Properties["address"].Pattern = addressPattern
			s.Properties["limit"].Minimum = ptr(1.0)
			s.Properties["limit"].Maximum = ptr(float64(investigation.MaxTxLimit))
			s.Properties["limit"].Default = []byte(fmt.Sprint(investigation.DefaultTxLimit))
			s.Properties["direction"].Enum = []any{"in", "out", "both"}
			s.Properties["direction"].Default = []byte(`"both"`)
		}),
		Annotations: readOnly("List recent wallet activity"),
	}, 0, func(in TransactionsInput) (string, string) { return in.Address, in.InvestigationID },
		func(ctx context.Context, in TransactionsInput) (investigation.TransactionsResult, error) {
			addr, err := blockchain.NormalizeAddress(in.Address)
			if err != nil {
				return investigation.TransactionsResult{}, err
			}
			return deref(d.Investigation.GetTransactions(ctx, addr, in.Direction, in.Limit))
		})

	registerTraceTool(s, d)

	addTool(s, d, &mcp.Tool{
		Name:  "check_sanctions",
		Title: "Screen address against OFAC SDN",
		Description: "Screen an Ethereum address against the loaded official OFAC SDN dataset (EVM-format digital currency " +
			"identifiers). status is match (with SDN entry name, uid, programs, listed asset and identifier), no_match " +
			"(dataset loaded, address absent) or inconclusive (dataset unavailable - nothing can be concluded). Includes " +
			"dataset publish date, source URL and last successful refresh. Direct identifier screening only; not legal clearance.",
		InputSchema: schemaFor[AddressInput](func(s *jsonschema.Schema) {
			s.Properties["address"].Pattern = addressPattern
		}),
		Annotations: readOnly("Screen address against OFAC SDN"),
	}, 0, func(in AddressInput) (string, string) { return in.Address, in.InvestigationID },
		func(ctx context.Context, in AddressInput) (sanctions.Result, error) {
			addr, err := blockchain.NormalizeAddress(in.Address)
			if err != nil {
				return sanctions.Result{}, err
			}
			return d.Sanctions.Check(ctx, addr), nil
		})

	addTool(s, d, &mcp.Tool{
		Name:  "risk_report",
		Title: "Explainable wallet risk report",
		Description: "Generate an explainable 0-100 rule-based risk score for an Ethereum address with evidence. Rules: " +
			"SANCTIONS_DIRECT (address on OFAC SDN, forces 100), SANCTIONS_ONE_HOP (sent to 60 / received from 40), " +
			"MIXER_INTERACTION (configured Tornado Cash contracts, 40/30), DEPOSITS_THEN_LARGE_OUTFLOW (25), " +
			"NEW_WALLET_LARGE_TRANSFER (20). Each finding has supporting tx hashes, counterparties, sources, confidence and " +
			"limitations; rule_evaluations shows triggered / not_triggered / not_evaluated for every rule. " +
			"assessment_status and score_is_lower_bound flag missing data: never treat an incomplete assessment as low risk. " +
			"Scores are indicators requiring review, not legal or criminal determinations.",
		InputSchema: schemaFor[AddressInput](func(s *jsonschema.Schema) {
			s.Properties["address"].Pattern = addressPattern
		}),
		Annotations: readOnly("Explainable wallet risk report"),
	}, d.RiskTimeout, func(in AddressInput) (string, string) { return in.Address, in.InvestigationID },
		func(ctx context.Context, in AddressInput) (risk.Report, error) {
			addr, err := blockchain.NormalizeAddress(in.Address)
			if err != nil {
				return risk.Report{}, err
			}
			return d.Risk.Report(ctx, addr), nil
		})
}

func registerTraceTool(s *mcp.Server, d *Deps) {
	addTool(s, d, &mcp.Tool{
		Name:  "trace_funds",
		Title: "Trace outgoing funds",
		Description: "Follow confirmed outgoing transfers from an address for 1-3 hops and return a directed graph: nodes " +
			"(addresses with hop, labels, flags such as ofac_sdn or mixer, and why expansion stopped) and edges (exact " +
			"aggregated amount, transfer count, supporting tx hashes, first/last seen). Traces one asset at a time. " +
			"flagged_nodes lists sanctioned/mixer addresses reached, with the discovery path. Bounded by node, edge, " +
			"API-call and time limits; partial=true plus warnings when a limit or provider failure cut the trace short. " +
			"This is graph tracing of where value was sent, not proof that specific coins moved along a path.",
		InputSchema: schemaFor[TraceInput](func(s *jsonschema.Schema) {
			s.Properties["address"].Pattern = addressPattern
			s.Properties["hops"].Minimum = ptr(1.0)
			s.Properties["hops"].Maximum = ptr(float64(d.Investigation.Trace.MaxHops))
			s.Properties["hops"].Default = []byte(fmt.Sprint(defaultTraceHops))
			s.Properties["min_amount"].Pattern = `^[0-9]*\.?[0-9]+$`
			s.Properties["min_amount"].Default = []byte(`"` + defaultTraceMin + `"`)
			s.Properties["asset"].Default = []byte(`"native"`)
		}),
		Annotations: readOnly("Trace outgoing funds"),
	}, d.Investigation.Trace.Timeout+15*time.Second,
		func(in TraceInput) (string, string) { return in.Address, in.InvestigationID },
		func(ctx context.Context, in TraceInput) (investigation.TraceResult, error) {
			addr, err := blockchain.NormalizeAddress(in.Address)
			if err != nil {
				return investigation.TraceResult{}, err
			}
			req := investigation.TraceRequest{Address: addr, Hops: in.Hops, MinAmount: in.MinAmount, Asset: in.Asset}
			if req.Hops == 0 {
				req.Hops = defaultTraceHops
			}
			if req.MinAmount == "" {
				req.MinAmount = defaultTraceMin
			}
			return deref(d.Investigation.TraceFunds(ctx, req))
		})
}

// deref adapts (*T, error) service results to the value types the SDK infers
// output schemas from.
func deref[T any](v *T, err error) (T, error) {
	if err != nil || v == nil {
		var zero T
		return zero, err
	}
	return *v, nil
}

func schemaFor[T any](edit func(*jsonschema.Schema)) *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	edit(s)
	return s
}

func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: ptr(true)}
}
