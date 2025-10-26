# Unibase X402 Facilitator on BNB chain

## About x402

The [x402 payment protocol](https://docs.cdp.coinbase.com/x402/docs/overview) is an HTTP-based payment protocol that enables developers running resource servers to accept payments from users using a variety of payment methods. Servers declare payment requirements for specific routes. Clients send cryptographically signed payment payloads. Facilitators verify and settle payments on-chain.

## x402 Facilitator

By building as the inaugural X402 facilitator on **BNB Chain**, **Unibase** unlocks the ability for AI agents to autonomously search, purchase, and pay. It utilizes the chain's low-cost and high-efficiency to bridge intelligent automation and real-world commerce.



|                    |                                                                                                                    |
| ------------------ | ------------------------------------------------------------------------------------------------------------------ |
| Base Url           | https://api.x402.unibase.com/                                                                                      |
| Supported Networks | `BSC mainnet`, `BSC testnet`                                                                                       |
| Payment Schemes    | `exact`                                                                                                            |
| Supported Assets   | `EIP-3009`                                                                                                         |
| Capabilities       | `Verify Payments` `Settle Payments` `Supported Endpoint` `List Resources`                                          |
| Methods            | - GET /<br>- GET /verify<br>- POST /verify<br>- GET /settle<br>- POST /settle<br>- GET /health<br>- GET /supported |
|                    |
