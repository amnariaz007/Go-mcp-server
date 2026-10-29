# WalletScope risk model (`walletscope-risk-v1`)

WalletScope produces an **explainable, rule-based indicator score** from 0 to 100.
It is not a probability of criminal activity, not a legal determination, and not
machine learning. Every point in the score traces back to a named rule, a
documented threshold and, where the rule depends on transactions, specific
transaction hashes.

Preferred language in all output: *"elevated indicators requiring review"*,
never *"this wallet is criminal"*.

## 1. Evidence collected per report

| Evidence | Source | Bound |
|---|---|---|
| Direct sanctions status | Loaded OFAC SDN dataset (EVM-format digital currency identifiers) | exact address match |
| Address profile | Blockscout `GET /addresses/{a}` | 1 call |
| Activity history | Blockscout transactions, internal transactions, ERC-20 token transfers (both directions) | `WALLETSCOPE_RISK_HISTORY_PAGES` pages × 50 items per source (default 4 → up to 200 per source) |
| Mixer list | `WALLETSCOPE_MIXER_ADDRESSES` (default: 6 Tornado Cash contracts) | configuration |

Only **confirmed** activity is used: successful transactions with non-zero value,
successful internal transfers, ERC-20 Transfer events, and successful
zero-value contract calls (for the interaction rules). Failed and pending
transactions never count as evidence.

### 1.1 Unsigned outgoing token events (address poisoning)

Token contracts can emit `Transfer(from = X, …)` events naming any address as
sender. Address-poisoning campaigns use this to plant fake "outgoing" transfers
from a victim to lookalike addresses (found during real client testing on
2026-10-29: fake tokens named `ETH` with provider reputation `ok`).

When the wallet is provably an EOA (it signed at least one top-level
transaction in the reviewed data), every outgoing ERC-20 event is tagged
`initiated_by_wallet`:

* `true` — the event is inside a transaction the wallet signed;
* `false` — the transactions read cover the event's time but none of them is the
  carrying transaction (approved spender, relayer, or spoofed token);
* absent — not determinable (e.g. contract wallets such as Safes, whose
  transactions are sent by owners).

Events with `initiated_by_wallet=false` are counted with direction
`unsigned_token_event` at the **passive tier** of the exposure rules and are
never used as the large outflow of `DEPOSITS_THEN_LARGE_OUTFLOW`.

## 2. Rules

| Rule ID | Title | Max points | Severity |
|---|---|---|---|
| `SANCTIONS_DIRECT` | Address is listed in the loaded OFAC SDN dataset | 100 (forces score = 100) | critical |
| `SANCTIONS_ONE_HOP` | Direct counterparty of a sanctioned address | 60 (sent to / called) · 40 (received from only) | high · medium |
| `MIXER_INTERACTION` | Interaction with a configured mixer contract | 40 (sent to / called) · 30 (received from only) | high · medium |
| `DEPOSITS_THEN_LARGE_OUTFLOW` | Many small incoming deposits followed by a much larger outflow | 25 | medium |
| `NEW_WALLET_LARGE_TRANSFER` | Recently created wallet moving a large ETH amount | 20 | low |

### 2.1 `SANCTIONS_DIRECT`
Triggered when the normalized address equals an EVM-format `Digital Currency
Address - <ASSET>` identifier in the loaded SDN file (ETH, USDT, USDC, ARB,
BSC, ETC … — EVM addresses are identical across EVM chains, so the listed
asset is reported). Evidence: SDN entry uid, name, programs, identifier uid,
listed value. Confidence: high (exact identifier match).

### 2.2 `SANCTIONS_ONE_HOP`
Triggered when a counterparty in confirmed activity within the reviewed window
is itself in the SDN index. **60** if the wallet sent value to or called a
sanctioned address (an action the wallet controlled); **40** if it only
received from one or appears only in unsigned token events with it (both can
be unsolicited — dusting, address poisoning). Evidence: up to 20
tx hashes, the counterparties and their SDN records. This never implies the
queried wallet is itself sanctioned.

### 2.3 `MIXER_INTERACTION`
Triggered when a counterparty is in the configured mixer set. **40** for
outgoing transfers/calls (deposits), **30** for incoming only (withdrawals
paid to the wallet, which arrive as internal transfers). The default list holds
Tornado Cash mainnet contracts; each address was checked against its Blockscout
verified-contract name on 2026-10-29:

| Address | Blockscout verified name | Label |
|---|---|---|
| `0x12D66f87A04A9E220743712cE6d9bB1B5616B8Fc` | `TornadoCash_Eth_01` | 0.1 ETH pool |
| `0x47CE0C6eD5B0Ce3d3A51fdb1C52DC66a7c3c2936` | `TornadoCash_eth` | 1 ETH pool |
| `0x910Cbd523D972eb0a6f4cAe4618aD62622b39DbF` | `TornadoCash_eth` | 10 ETH pool |
| `0xA160cdAB225685dA1d56aa342Ad8841c3b53f291` | `TornadoCash_eth` | 100 ETH pool |
| `0xd90e2f925DA726b50C4Ed8D0Fb90Ad053324F31b` | `TornadoRouter` | Router |
| `0x722122dF12D4e14e13Ac3b6895a86e84145b6967` | `TornadoProxy` | Proxy |

Tornado Cash was removed from the SDN list in March 2025; it does **not**
appear in the current SDN file, which is why mixers are a separate,
configurable rule. Mixer use has legitimate privacy uses; the finding is an
indicator for review only.

### 2.4 `DEPOSITS_THEN_LARGE_OUTFLOW`
Evaluated per asset (native ETH and each ERC-20 contract separately; tokens the
provider marks with a non-`ok` reputation are skipped). For each confirmed
outgoing transfer *O* of amount *A* at time *t*, let *D* be the confirmed
incoming transfers of the same asset in `[t − W, t)` with amount
`0 < d ≤ A / M`. Triggered when

* `|D| ≥ N` and
* `sum(D) ≥ C% × A`.

Defaults: `N = 5` (`WALLETSCOPE_STRUCTURING_MIN_DEPOSITS`), `W = 72h`
(`…_WINDOW`), `M = 3` (`…_MULTIPLE`), `C = 50` (`…_COVERAGE_PCT`).
If several outflows qualify, the one with the most qualifying deposits is
reported (ties: larger outflow, then earlier time, then tx hash), so the result
is deterministic. Evidence: the outflow hash plus up to 20 deposit hashes. All
arithmetic is exact (`math/big`); assets are never mixed. Limitation: payroll,
exchange hot-wallet sweeps and merchants produce the same shape.

### 2.5 `NEW_WALLET_LARGE_TRANSFER`
Requires knowing the wallet's first activity, which is only possible when the
full indexed history was read. Triggered when the first observed activity is
within `WALLETSCOPE_NEW_WALLET_AGE` (default 30 days) of report time and the
wallet has a confirmed native ETH transfer (in or out) of at least
`WALLETSCOPE_LARGE_TRANSFER_ETH` (default 10 ETH). Evidence: the largest such
transfers (up to 5 hashes).
If the history is truncated but the oldest activity read is already older than
the age threshold, the rule is reported **not triggered** (the wallet is
provably not new). Otherwise it is **not evaluated**. ERC-20 amounts are not
used because WalletScope has no price oracle.

## 3. Aggregation

1. Each rule produces at most **one** finding; its points are the maximum
   applicable tier (e.g. 60 if any outgoing sanctioned exposure, else 40).
2. **Overlap handling**: a counterparty that is both sanctioned and a configured
   mixer contributes only to `SANCTIONS_ONE_HOP`. If that leaves the mixer rule
   with no other evidence, the mixer finding is still shown with `points = 0`
   and `suppressed_reason`.
3. A transaction hash may support several *different* rules (they measure
   different things); within one rule each hash is listed once.
4. `score = 100` if `SANCTIONS_DIRECT` is triggered; otherwise
   `score = min(100, Σ points)`.
5. The same evidence and configuration always produce the same score, findings
   and ordering (findings are ordered by points desc, then rule ID).

## 4. Bands

| Score | Band | Label |
|---|---|---|
| 75–100 | `severe` | Severe indicators requiring priority review |
| 50–74 | `high` | High indicators requiring review |
| 25–49 | `moderate` | Elevated indicators requiring review |
| 1–24 | `low` | Low indicators; review if other context warrants |
| 0, complete assessment | `none_detected` | No indicators detected by the configured rules |
| 0, partial assessment | `none_detected_in_reviewed_window` | No indicators in the reviewed window; older activity not assessed |
| 0, insufficient data | `indeterminate` | Indeterminate: required data could not be obtained |

## 5. Completeness

`assessment_status`:

* `complete` — sanctions dataset loaded, address profile and every activity
  source read to its last page.
* `partial` — sanctions dataset loaded and at least one activity source read,
  but history was truncated by the page budget or a source failed.
* `insufficient_data` — sanctions dataset unavailable **or** every activity
  source failed.

`score_is_lower_bound = true` whenever the status is not `complete`: missing
evidence can only hide indicators, never prove their absence. A score of 0 with
missing data is reported as `indeterminate` or
`none_detected_in_reviewed_window`, never as low risk.

## 6. Known limitations

* Provider index coverage: Blockscout's public instance may lag or omit data.
* Reviewed window: busy wallets are only assessed over their most recent
  activity (see `reviewed_window`).
* One-hop only: exposure through intermediaries is visible with `trace_funds`
  but is not scored.
* Sanctions coverage: only identifiers OFAC has published; the consolidated
  (non-SDN) lists are not loaded.
* Weights are engineering judgements, not calibrated probabilities.
