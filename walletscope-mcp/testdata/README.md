# Test fixtures

| Path | Origin |
|---|---|
| `blockscout/address_vitalik.json`, `counters_vitalik.json` | Recorded from `https://eth.blockscout.com/api/v2` on 2026-10-29, unmodified. |
| `blockscout/transactions_page1.json`, `token_transfers_page1.json`, `internal_transactions_tornado10.json` | Recorded on 2026-10-29 and **trimmed**: first 2-3 items only; large fields (`raw_input`, `decoded_input`, icons, fees) removed. Values that remain are as returned. |
| `blockscout/transactions_edge_cases.json` | **Synthetic**, in Blockscout's shape, to exercise failed / pending / contract-creation handling. Hashes are fabricated. |
| `sanctions/sdn_sample.xml` | **Excerpt** of the official OFAC SDN XML (2026-10-28 publication): two real entries copied verbatim plus one synthetic entry marked `SYNTHETIC` used for scenario tests. |

Risk-rule scenario tests use in-memory synthetic activity built in Go test code
(`internal/risk/*_test.go`); every synthetic address there is obviously
artificial (e.g. `0x1111…`).
