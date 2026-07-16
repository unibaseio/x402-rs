// Command facilitator is a standalone x402 v2 facilitator that speaks the
// batch-settlement EVM scheme. It exposes the three standard x402 endpoints —
// GET /supported, POST /verify, POST /settle — that resource servers and
// clients talk to.
//
// In the subscription model, this service is the neutral relayer:
//   - It submits the onchain `deposit` when a subscriber opens a channel.
//   - It verifies each off-chain voucher (a signature check, no chain call).
//   - It submits batched `claimWithSignature` + `settle` transactions when the
//     resource server decides to sweep accumulated usage onchain.
//   - It submits `refundWithSignature` when a subscriber cancels and reclaims
//     their unused prepaid balance.
//
// The facilitator holds no custody: subscribers' funds live in the onchain
// escrow contract, and every state transition is authorized by an EIP-712
// signature from the payer or the receiver's authorizer.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/joho/godotenv"
	x402 "github.com/x402-foundation/x402/go/v2"
	batchsettlement "github.com/x402-foundation/x402/go/v2/mechanisms/evm/batch-settlement"
	batchedfac "github.com/x402-foundation/x402/go/v2/mechanisms/evm/batch-settlement/facilitator"
)

const defaultPort = "4022"

func main() {
	_ = godotenv.Load()

	port := envOr("PORT", defaultPort)

	evmPrivateKey := os.Getenv("EVM_PRIVATE_KEY")
	if evmPrivateKey == "" {
		fmt.Println("EVM_PRIVATE_KEY environment variable is required")
		os.Exit(1)
	}
	rpcURL := envOr("EVM_RPC_URL", "https://sepolia.base.org")

	// The evmSigner is the wallet that actually submits transactions onchain
	// (deposit / claim / settle / refund). It pays gas, so it needs a small
	// balance of the native token. It never touches subscriber funds directly.
	evmSigner, err := newFacilitatorEvmSigner(evmPrivateKey, rpcURL)
	if err != nil {
		fmt.Printf("Failed to create EVM signer: %v\n", err)
		os.Exit(1)
	}

	// The receiverAuthorizer signs ClaimBatch / Refund EIP-712 messages and is
	// advertised under /supported so servers can delegate to it. If a dedicated
	// key isn't supplied, the facilitator advertises no authorizer and servers
	// must run their own (the recommended production path — see the READMEs).
	var authorizer batchsettlement.AuthorizerSigner
	if authKey := os.Getenv("EVM_RECEIVER_AUTHORIZER_PRIVATE_KEY"); authKey != "" {
		authorizer, err = newAuthorizerSigner(authKey)
		if err != nil {
			fmt.Printf("Failed to create authorizer signer: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Printf("EVM Facilitator account: %s\n", evmSigner.GetAddresses()[0])
	if authorizer != nil {
		fmt.Printf("EVM Receiver Authorizer: %s\n", authorizer.Address())
	} else {
		fmt.Println("EVM Receiver Authorizer: not configured (servers must self-manage)")
	}

	// Wire up the facilitator with the batch-settlement scheme on Base Sepolia.
	// Add more networks (e.g. "eip155:8453" for Base mainnet) here as needed.
	facilitator := x402.Newx402Facilitator()
	facilitator.Register(
		[]x402.Network{"eip155:84532"},
		batchedfac.NewBatchSettlementEvmScheme(evmSigner, authorizer),
	)

	// Observability hooks — swap for structured logging / metrics in production.
	facilitator.OnAfterVerify(func(ctx x402.FacilitatorVerifyResultContext) error {
		fmt.Printf("[verify] ok\n")
		return nil
	})
	facilitator.OnAfterSettle(func(ctx x402.FacilitatorSettleResultContext) error {
		fmt.Printf("[settle] tx=%s\n", ctx.Result.Transaction)
		return nil
	})

	mux := http.NewServeMux()

	// GET /supported — advertises the schemes, networks, and (optionally) the
	// receiverAuthorizer address that clients and servers discover at startup.
	mux.HandleFunc("GET /supported", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, facilitator.GetSupported())
	})

	// POST /verify — cheap, off-chain validation of a payment payload against
	// its requirements. For batch-settlement this is a voucher signature check
	// plus a channel-state lookup; no transaction is submitted.
	mux.HandleFunc("POST /verify", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		payload, requirements, err := readVerifyBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		result, err := facilitator.Verify(ctx, payload, requirements)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	// POST /settle — submits the onchain transaction(s). For batch-settlement
	// the resource server drives this via its ChannelManager (deposit on first
	// request, then batched claim/settle, and refunds on cancellation).
	mux.HandleFunc("POST /settle", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()

		payload, requirements, err := readVerifyBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		result, err := facilitator.Settle(ctx, payload, requirements)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	fmt.Printf("Facilitator listening on http://localhost:%s\n", port)
	fmt.Println("  GET  /supported")
	fmt.Println("  POST /verify")
	fmt.Println("  POST /settle")
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		fmt.Printf("Server error: %v\n", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func readVerifyBody(r *http.Request) (json.RawMessage, json.RawMessage, error) {
	var body struct {
		PaymentPayload      json.RawMessage `json:"paymentPayload"`
		PaymentRequirements json.RawMessage `json:"paymentRequirements"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if len(body.PaymentPayload) == 0 || len(body.PaymentRequirements) == 0 {
		return nil, nil, fmt.Errorf("missing paymentPayload or paymentRequirements")
	}
	return body.PaymentPayload, body.PaymentRequirements, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
