// Package sanctions screens addresses against a loaded public sanctions
// dataset. Screening reports only direct matches supported by loaded records;
// it is not legal compliance clearance.
package sanctions

import (
	"context"
	"time"
)

// Screening outcomes.
const (
	StatusMatch        = "match"        // address appears in the loaded dataset
	StatusNoMatch      = "no_match"     // dataset loaded successfully and address not present
	StatusInconclusive = "inconclusive" // no usable dataset; nothing can be concluded
)

// Disclaimer accompanies every screening result.
const Disclaimer = "Screening against the loaded public OFAC SDN dataset only (direct address identifiers). " +
	"Not legal advice or compliance clearance; a no_match does not mean the address is unconnected to a sanctioned person, " +
	"and entities may control addresses that OFAC has not listed."

// Record is one sanctions-list entry that lists a matching address.
type Record struct {
	UID         string   `json:"uid" jsonschema:"OFAC SDN entry uid"`
	Name        string   `json:"name"`
	Type        string   `json:"type" jsonschema:"Individual, Entity, Vessel or Aircraft"`
	Programs    []string `json:"programs"`
	Aliases     []string `json:"aliases,omitempty"`
	IDUID       string   `json:"id_uid" jsonschema:"uid of the address identifier within the entry"`
	IDType      string   `json:"id_type" jsonschema:"e.g. Digital Currency Address - ETH"`
	ListedAsset string   `json:"listed_asset" jsonschema:"asset code OFAC listed the address under (ETH, USDT, ...)"`
	ListedValue string   `json:"listed_value" jsonschema:"address exactly as written in the dataset"`
}

// DatasetInfo describes the loaded dataset.
type DatasetInfo struct {
	Name               string     `json:"name"`
	SourceURL          string     `json:"source_url"`
	PublishDate        string     `json:"publish_date,omitempty" jsonschema:"Publish_Date from the file"`
	Entries            int        `json:"entries"`
	EVMAddresses       int        `json:"evm_addresses" jsonschema:"number of 0x-format digital currency identifiers indexed"`
	LastSuccessfulLoad *time.Time `json:"last_successful_load,omitempty"`
	LastAttempt        *time.Time `json:"last_attempt,omitempty"`
	LastError          string     `json:"last_error,omitempty"`
	Stale              bool       `json:"stale" jsonschema:"true when the last successful refresh is older than twice the refresh interval"`
}

// Result is the check_sanctions response.
type Result struct {
	Address    string      `json:"address"`
	Status     string      `json:"status" jsonschema:"match, no_match or inconclusive"`
	Matches    []Record    `json:"matches"`
	Reason     string      `json:"reason,omitempty" jsonschema:"why the result is inconclusive"`
	Dataset    DatasetInfo `json:"dataset"`
	CheckedAt  time.Time   `json:"checked_at"`
	Disclaimer string      `json:"disclaimer"`
}

// IsPartial treats inconclusive screening as partial for audit logging.
func (r Result) IsPartial() bool { return r.Status == StatusInconclusive }

// Screener checks addresses against a sanctions dataset.
type Screener interface {
	// Check screens addr (EIP-55 normalized), waiting briefly for an initial
	// load if necessary. It never returns no_match without a loaded dataset.
	Check(ctx context.Context, addr string) Result
	// Lookup is a non-blocking in-memory lookup; loaded=false means no dataset.
	Lookup(addr string) (records []Record, loaded bool)
	Info() DatasetInfo
}
