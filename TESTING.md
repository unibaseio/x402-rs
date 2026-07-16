# Deploying & testing YOUR OWN facilitator locally

Everything below runs your local facilitator (`localhost:4022`) and points the server + client
at it. The chain underneath is Base Sepolia testnet (free funds).

## 0. Generate test wallets

```bash
cd tools/keygen
go run . 4
```

You need up to **4** keys. Only two need funding:

| Role | Env var | Where used | Funding |
|------|---------|------------|---------|
| Facilitator | `EVM_PRIVATE_KEY` (facilitator) | pays gas for every tx | **ETH** on Base Sepolia |
| Subscriber | `EVM_PRIVATE_KEY` (client) | prepaid balance | **USDC** on Base Sepolia |
| Receiver | `EVM_ADDRESS` (server) | where revenue settles | none (just an address) |
| Receiver authorizer | `EVM_RECEIVER_AUTHORIZER_PRIVATE_KEY` (server) | signs claims/refunds | none |

Fund them:
- ETH faucet (Base Sepolia): e.g. <https://www.alchemy.com/faucets/base-sepolia>
- USDC faucet: <https://faucet.circle.com> (pick Base Sepolia)

## 1. Start your facilitator

```bash
cd facilitator
cp .env.example .env
# set EVM_PRIVATE_KEY = facilitator key (funded with ETH)
go run .
```

Expected:
```
EVM Facilitator account: 0x....
Facilitator listening on http://localhost:4022
```

### Test it in isolation (no funds needed)

`GET /supported` — should list the batch-settlement scheme:
```bash
curl -s http://localhost:4022/supported | jq
# {
#   "kinds": [{"x402Version":2,"scheme":"batch-settlement","network":"eip155:84532"}],
#   ...
# }
```

`POST /verify` with a deliberately empty payload — should return a structured error (proves the
endpoint is live and it's *your* process handling it; watch the facilitator's stdout):
```bash
curl -s -X POST http://localhost:4022/verify \
  -H 'Content-Type: application/json' \
  -d '{"paymentPayload":{},"paymentRequirements":{}}' | jq
```

## 2. Start the server, pointed at YOUR facilitator

```bash
cd server
cp .env.example .env
#   EVM_ADDRESS                         = receiver address
#   FACILITATOR_URL=http://localhost:4022   ← your local facilitator
#   EVM_RECEIVER_AUTHORIZER_PRIVATE_KEY = authorizer key (recommended)
go run .
```

Expected:
```
Unibase Pro subscription API listening at http://localhost:4021
```

## 3. Run the subscriber client

```bash
cd client
cp .env.example .env
#   EVM_PRIVATE_KEY = subscriber key (funded with USDC)
#   RESOURCE_SERVER_URL=http://localhost:4021
#   NUMBER_OF_REQUESTS=5
go run .
```

## 4. Confirm it's really YOUR facilitator doing the work

The proof is in the **facilitator terminal**. As the client runs, you should see:
```
[verify] ok            # one per request (off-chain voucher check)
[settle] tx=0x....      # deposit tx (request 1) + batched claim/settle from the server
```
And in the **server terminal**:
```
[claim]  N vouchers folded onchain (tx: 0x....)
[settle] swept to 0x.... (tx: 0x....)
```

Because `FACILITATOR_URL` points only at `localhost:4022`, every `/verify` and `/settle` is
handled by your process — there is no fallback to a public facilitator.

## 5. Test cancellation / refund

In `client/.env` set:
```
REFUND_AFTER_REQUESTS=true
# REFUND_AMOUNT=   (empty = full refund of remaining balance)
```
Re-run the client; you'll see a `refundWithSignature` tx in the facilitator log and the unused
balance returned to the subscriber.

---

## Troubleshooting

| Symptom | Cause / fix |
|---------|-------------|
| `insufficient funds` on first request | Facilitator wallet has no ETH (it pays gas). Fund it. |
| Verify fails / voucher rejected | Subscriber has no USDC. Stale local state (e.g. after a refund) is auto-resynced from chain — the client prints `[recover] channel state resynced` and retries once. |
| `get chain ID` error on facilitator start | RPC unreachable; check `EVM_RPC_URL`. |
| Server prints `Waiting for facilitator…` | Normal — it polls `/supported` for up to 60s so startup order doesn't matter. If it exits, the facilitator never came up on 4022. |

## Fully offline (no testnet) — advanced

To avoid the testnet entirely, run a local Anvil chain and deploy the batch-settlement contract
to it:
```bash
anvil --port 8545            # local chain, pre-funded accounts
```
Then deploy the batch-settlement escrow contract from
[`x402-foundation/x402/contracts`](https://github.com/x402-foundation/x402/tree/main/contracts)
to that chain, register a network entry for the local chain ID, and set every `EVM_RPC_URL` to
`http://localhost:8545`. This is more setup (contract deployment + a custom network config) and
only worth it for CI or air-gapped testing — for normal local dev, Base Sepolia is simpler.
