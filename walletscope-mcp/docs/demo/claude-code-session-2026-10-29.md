# Real Claude Code session — 2026-10-29

**Captured, not edited.** Headless Claude Code (`claude -p`, version 2.1.312, model `claude-opus-5-5`)
connected to the locally built `bin/walletscope` over **stdio** using a temporary MCP config
(`--mcp-config mcp.json --strict-mcp-config`), restricted to the five WalletScope tools.
MCP server status reported by the client: `[{'name': 'walletscope', 'status': 'connected', 'source': 'dynamic'}]`.

Prompt:

> Investigate this Ethereum wallet: 0x212B8e1f935B779bBa6DD6Ca7c29c30aAa6e37BB. Show me where its funds went,
> check for sanctions exposure, and explain its risk score with transaction evidence. Use only the walletscope tools.

## Tool calls made by Claude (5)

| # | Tool | Arguments |
|---|---|---|
| 1 | `get_address_info` | `{"address": "0x212B8e1f935B779bBa6DD6Ca7c29c30aAa6e37BB", "investigation_id": "inv-212b8e1f"}` |
| 2 | `get_transactions` | `{"address": "0x212B8e1f935B779bBa6DD6Ca7c29c30aAa6e37BB", "limit": 100, "investigation_id": "inv-212b8e1f"}` |
| 3 | `check_sanctions` | `{"address": "0x212B8e1f935B779bBa6DD6Ca7c29c30aAa6e37BB", "investigation_id": "inv-212b8e1f"}` |
| 4 | `trace_funds` | `{"address": "0x212B8e1f935B779bBa6DD6Ca7c29c30aAa6e37BB", "hops": 2, "investigation_id": "inv-212b8e1f"}` |
| 5 | `risk_report` | `{"address": "0x212B8e1f935B779bBa6DD6Ca7c29c30aAa6e37BB", "investigation_id": "inv-212b8e1f"}` |

## Claude's final answer (7 turns)

> Note: this session ran **before** the address-poisoning fix. Claude's section 4 correctly observed that
> WalletScope then reported spoofed fake-`ETH` token events as confirmed outgoing transfers. That observation led
> to the `initiated_by_wallet` attribution and the passive-tier handling described in `docs/risk-model.md` §1.1;
> re-running `get_transactions` on this wallet now returns `initiated_by_wallet: false` for all six spoofed events.

## Wallet 0x212B…37BB: investigation summary

**Bottom line:** This wallet is about 1.5 hours old. Its funds came out of the Tornado Cash 10 ETH pool, and about 9.96 ETH passed through two cross-chain services, Chainflip and then NEAR Intents, in roughly 12 minutes. It is not sanctioned itself, and none of its direct counterparties are on the current OFAC list. The risk score is **30/100 (moderate)**. All data sources reported complete coverage (`complete=true`, `partial=false`), so this is not a score held down by missing data.

### 1. Profile
- An ordinary wallet (EOA). Remaining balance is **0.000003 ETH**, so it has effectively been emptied.
- First activity was 2026-10-29 10:47:59 UTC and the last was 12:03:23 UTC. It has no ENS name and no labels.
- It signed only 2 transactions of its own, both outgoing ETH sends. Everything else was sent to it by other addresses or is fake token events (section 4).

### 2. Where the funds went

| Time (UTC) | Step | Amount | Tx |
|---|---|---|---|
| 10:47:59 | **In** from Tornado Cash 10 ETH pool `0x910C…9DbF` | 9.962318 ETH | `0xbfe54a96…f3b4` |
| 10:53:47 | **Out** to `0xEc00…e079` (labelled "Deposit") | 9.962314 ETH | `0x1027120f…eeca` |
| 10:56:59 | **In** from Chainflip Vault `0xF5e1…2Bcc` | 9.962287 ETH | `0xbea5decd…649f` |
| 10:59:23 | **Out** to `0x55ca…9473` | 9.962285 ETH | `0xefa65330…eca4` |
| 11:00:11 | Hop 2: `0x55ca…9473` sends to `0x2CfF…2680` (**NEAR Intents**) | 9.962285 ETH | `0xe6af4b22…31c7` |

- The ETH it received roughly 10 ETH out of Tornado (the 10 ETH pool minus a fee) and went straight into what looks like a Chainflip deposit address. About 3 minutes later almost the same amount came back from the Chainflip Vault. That is a swap round-trip, and the round trip is my reading of the timing, not something the tool states.
- The wallet then sent the ETH to a middle wallet, which passed all of it to NEAR Intents 48 seconds later. The trace stops there: NEAR Intents is a cross-chain service, so the next step is probably on another chain and outside what these tools can see.
- Total in was 19.924605 ETH and total out was 19.924599 ETH. That counts the ~10 ETH twice because it went out and came back. The gap matches the gas spent.
- The tools only show where value was sent. They don't prove the same coins moved along that path.

### 3. Sanctions exposure
- **Direct check: no match** against the OFAC SDN list published 10/28/2026 (137 Ethereum addresses on it, data not stale).
- **One-hop check (`SANCTIONS_ONE_HOP`): not triggered.**
- Blockscout's tags on the Tornado pool still say "OFAC Sanctions Lists / SANCTIONED / BLOCKED". The OFAC list itself doesn't include that address. I believe Treasury removed Tornado Cash from the list in March 2025, which would mean those tags are out of date. Whether your organisation still restricts Tornado exposure is a policy question.
- No sanctioned or mixer addresses turned up in the 2-hop trace (`flagged_nodes: []`).

### 4. Address-poisoning spam (ignore when tracing)
Six "outgoing" token transfers and two zero-value calls in the history are fake:
- **Lookalike addresses:** `0x55Ca AEEa…9473` and `0xeC00 32d0…E079` sent zero-value calls (`0xad6b17a1…`, `0x0e82d1c3…`). They copy the first and last characters of the wallet's real counterparties `0x55ca…9473` and `0xEc00…e079`.
- **Fake tokens:** a token named "ETH" (`0x37ed…F6F7`) and a lookalike "ЕΤН" (`0xE71A…DFAD`, built from Cyrillic and Greek letters plus hidden characters). Their transfers appear to send ~9.96 to other lookalikes: `0xEc00 3b7D…0079`, `0xEC00 050D…E079`, `0x55cA 5c27…9473` and `0x55ca df85…0473`.
- The wallet only has 4 transactions of its own, and none of these token events come from them, so it never signed them. The tool marks them `reputation: ok` and `transfer_confirmed: true` anyway, which is wrong. They are bait to trick the owner into copying a lookalike address, and they are not real fund flows.

### 5. Risk score: 30/100
| Rule | Result | Evidence |
|---|---|---|
| `MIXER_INTERACTION` | **+30, triggered** (high confidence) | Withdrawal from Tornado 10 ETH pool, tx `0xbfe54a96…f3b4` |
| `SANCTIONS_DIRECT` | not triggered | Not on the list |
| `SANCTIONS_ONE_HOP` | not triggered | No listed counterparties |
| `DEPOSITS_THEN_LARGE_OUTFLOW` | not triggered | — |
| `NEW_WALLET_LARGE_TRANSFER` | not triggered | Wallet is new, but no send reached the rule's size threshold |

**My view:** the score is accurate under the tool's rules, but it understates the pattern. Everything below is my interpretation, not a tool finding. The wallet is brand new, its money came only from a Tornado withdrawal, and that money went through a cross-chain swap, a pass-through wallet, and a cross-chain intents service within 12 minutes. The wallet ends empty. That is consistent with layering, meaning efforts to obscure where funds came from. The mixer rule also allows for legitimate privacy use, so none of this proves wrongdoing.

### Gaps and suggested next steps
- Chainflip and NEAR Intents both move funds to other chains, and these tools cover Ethereum only. Follow-up would need data from those services, such as Chainflip swap records or NEAR Intents settlement records.
- The trace stopped at contracts it doesn't expand (the Chainflip deposit address) and at the 2-hop limit (NEAR Intents).
- The address labels come from Blockscout and don't establish who owns any address.

All calls used investigation ID `inv-212b8e1f`, so they can be matched up in the audit logs.
