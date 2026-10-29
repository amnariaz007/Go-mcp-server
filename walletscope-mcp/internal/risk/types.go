// Package risk evaluates deterministic, explainable risk rules over collected
// evidence. See docs/risk-model.md for the authoritative model description.
package risk

import (
	"math/big"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
	"github.com/amnariaz007/walletscope-mcp/internal/investigation"
	"github.com/amnariaz007/walletscope-mcp/internal/sanctions"
)

// ModelVersion identifies the scoring model.
const ModelVersion = "walletscope-risk-v1"

// Rule IDs.
const (
	RuleSanctionsDirect = "SANCTIONS_DIRECT"
	RuleSanctionsOneHop = "SANCTIONS_ONE_HOP"
	RuleMixer           = "MIXER_INTERACTION"
	RuleStructuring     = "DEPOSITS_THEN_LARGE_OUTFLOW"
	RuleNewWallet       = "NEW_WALLET_LARGE_TRANSFER"
)

// Points per rule tier (docs/risk-model.md §2).
const (
	PointsSanctionsDirect   = 100
	PointsOneHopOutgoing    = 60
	PointsOneHopIncoming    = 40
	PointsMixerOutgoing     = 40
	PointsMixerIncoming     = 30
	PointsStructuring       = 25
	PointsNewWalletTransfer = 20
	maxEvidenceHashes       = 20
)

// Rule evaluation statuses.
const (
	EvalTriggered    = "triggered"
	EvalNotTriggered = "not_triggered"
	EvalNotEvaluated = "not_evaluated"
)

// Assessment statuses.
const (
	AssessmentComplete     = "complete"
	AssessmentPartial      = "partial"
	AssessmentInsufficient = "insufficient_data"
)

// Config holds rule thresholds.
type Config struct {
	Mixers                 map[string]string // checksummed address -> label
	StructuringMinDeposits int
	StructuringWindow      time.Duration
	StructuringMultiple    int
	StructuringCoveragePct int
	NewWalletAge           time.Duration
	LargeTransferWei       *big.Int
}

// Evidence is everything the engine needs; it performs no I/O.
type Evidence struct {
	Address        string
	Now            time.Time
	Direct         sanctions.Result
	SanctionsInfo  sanctions.DatasetInfo
	Lookup         func(addr string) []sanctions.Record // nil when dataset not loaded
	History        *investigation.History
	HistoryRead    bool // false if history collection was not attempted
	AddressInfo    *blockchain.AddressInfo
	AddressError   string
	ProviderName   string
	ProviderURL    string
	HistoryFetched time.Time
}

// Counterparty is an address implicated by a finding.
type Counterparty struct {
	Address   string             `json:"address"`
	Role      string             `json:"role" jsonschema:"why this counterparty matters, e.g. sanctioned or mixer label"`
	Direction string             `json:"direction" jsonschema:"sent_to, received_from, unsigned_token_event or both, relative to the queried wallet"`
	Records   []sanctions.Record `json:"sanctions_records,omitempty"`
	TxCount   int                `json:"tx_count"`
}

// TimeRange bounds the evidence timestamps.
type TimeRange struct {
	First *time.Time `json:"first,omitempty"`
	Last  *time.Time `json:"last,omitempty"`
}

// Finding is one triggered (or suppressed) rule.
type Finding struct {
	RuleID            string             `json:"rule_id"`
	Title             string             `json:"title"`
	Severity          string             `json:"severity" jsonschema:"critical, high, medium or low"`
	Points            int                `json:"points" jsonschema:"points contributed to the score"`
	MaxPoints         int                `json:"max_points"`
	Explanation       string             `json:"explanation"`
	EvidenceTxHashes  []string           `json:"evidence_tx_hashes"`
	EvidenceTruncated bool               `json:"evidence_truncated,omitempty"`
	Counterparties    []Counterparty     `json:"counterparties,omitempty"`
	SanctionsRecords  []sanctions.Record `json:"sanctions_records,omitempty"`
	EvidenceTimeRange *TimeRange         `json:"evidence_time_range,omitempty"`
	Metrics           map[string]string  `json:"metrics,omitempty"`
	Sources           []string           `json:"sources"`
	Confidence        string             `json:"confidence" jsonschema:"strength of the evidence: high, medium or low, with reason"`
	Limitations       []string           `json:"limitations"`
	SuppressedReason  string             `json:"suppressed_reason,omitempty"`
}

// RuleEvaluation records the outcome of every rule, including rules that could
// not be evaluated, so absence of a finding is never ambiguous.
type RuleEvaluation struct {
	RuleID string `json:"rule_id"`
	Status string `json:"status" jsonschema:"triggered, not_triggered or not_evaluated"`
	Reason string `json:"reason,omitempty"`
}

// DataSource documents provenance.
type DataSource struct {
	Name        string     `json:"name"`
	URL         string     `json:"url,omitempty"`
	RetrievedAt *time.Time `json:"retrieved_at,omitempty"`
	Detail      string     `json:"detail,omitempty"`
}

// Report is the risk_report response.
type Report struct {
	Chain              string                         `json:"chain"`
	Address            string                         `json:"address"`
	GeneratedAt        time.Time                      `json:"generated_at"`
	ModelVersion       string                         `json:"model_version"`
	Score              int                            `json:"score" jsonschema:"0-100 rule-based indicator score (not a probability)"`
	ScoreIsLowerBound  bool                           `json:"score_is_lower_bound" jsonschema:"true when missing data could hide further indicators"`
	RiskBand           string                         `json:"risk_band"`
	RiskBandLabel      string                         `json:"risk_band_label"`
	AssessmentStatus   string                         `json:"assessment_status" jsonschema:"complete, partial or insufficient_data"`
	Summary            string                         `json:"summary"`
	Findings           []Finding                      `json:"findings"`
	RuleEvaluations    []RuleEvaluation               `json:"rule_evaluations"`
	EvidenceTxHashes   []string                       `json:"evidence_tx_hashes" jsonschema:"union of all finding evidence hashes"`
	DataSources        []DataSource                   `json:"data_sources"`
	Coverage           []investigation.SourceCoverage `json:"coverage"`
	ReviewedWindow     *TimeRange                     `json:"reviewed_window,omitempty" jsonschema:"time span of the activity actually reviewed"`
	ActivitiesReviewed int                            `json:"activities_reviewed"`
	Warnings           []string                       `json:"warnings"`
	Limitations        []string                       `json:"limitations"`
}

// IsPartial reports incomplete assessments for audit logging.
func (r Report) IsPartial() bool { return r.AssessmentStatus != AssessmentComplete }
