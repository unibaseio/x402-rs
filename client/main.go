// Command client is a subscriber to the Unibase Pro metered API.
//
// Subscription lifecycle demonstrated here:
//  1. SUBSCRIBE  — the first request auto-opens a payment channel by depositing
//     `maxPricePerCall × DEPOSIT_MULTIPLIER` into the onchain escrow. That
//     deposit is the subscriber's prepaid balance.
//  2. USE        — every subsequent request is a pure off-chain voucher: the
//     client bumps a cumulative signed amount and sends it in the payment
//     header. No transaction, no gas, sub-second latency.
//  3. CANCEL     — optionally request a cooperative refund of the unused
//     balance (full or partial).
//
// The x402 client SDK handles deposit, voucher signing, channel-state recovery,
// and 402 resync transparently — the app code below is just an HTTP loop.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/joho/godotenv"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	batchsettlement "github.com/x402-foundation/x402/go/v2/mechanisms/evm/batch-settlement"
	batchedclient "github.com/x402-foundation/x402/go/v2/mechanisms/evm/batch-settlement/client"
	"github.com/x402-foundation/x402/go/v2/types"
	evmsigners "github.com/x402-foundation/x402/go/v2/signers/evm"
)

func main() {
	_ = godotenv.Load()

	evmPrivateKey := os.Getenv("EVM_PRIVATE_KEY")
	if evmPrivateKey == "" {
		fmt.Println("EVM_PRIVATE_KEY environment variable is required")
		os.Exit(1)
	}

	baseURL := envOr("RESOURCE_SERVER_URL", "http://localhost:4021")
	endpointPath := envOr("ENDPOINT_PATH", "/v1/insights")
	url := baseURL + endpointPath

	rpcURL := envOr("EVM_RPC_URL", "https://sepolia.base.org")
	channelSalt := envOr("CHANNEL_SALT", batchedclient.DefaultSalt)
	storageDir := os.Getenv("STORAGE_DIR")
	numberOfRequests := atoiOr("NUMBER_OF_REQUESTS", 5)
	depositMultiplier := atoiOr("DEPOSIT_MULTIPLIER", batchedclient.DefaultDepositMultiplier)
	refundAfterRequests := os.Getenv("REFUND_AFTER_REQUESTS") == "true"
	refundAmount := os.Getenv("REFUND_AMOUNT")

	// Dial RPC so the signer can recover onchain channel state on a cold start.
	// Without it, a fresh run against an existing channel would sign a voucher
	// with a stale cumulative base and the facilitator would reject it.
	ethClient, err := ethclient.Dial(rpcURL)
	if err != nil {
		fmt.Printf("Failed to dial EVM RPC %s: %v\n", rpcURL, err)
		os.Exit(1)
	}
	defer ethClient.Close()

	signer, err := evmsigners.NewClientSignerFromPrivateKeyWithClient(evmPrivateKey, ethClient)
	if err != nil {
		fmt.Printf("Failed to create signer: %v\n", err)
		os.Exit(1)
	}

	cfg := &batchedclient.BatchSettlementEvmSchemeOptions{
		DepositMultiplier: depositMultiplier,
		Salt:              channelSalt,
	}

	// Optional dedicated voucher-signing key. Recommended for smart wallets:
	// lets the facilitator verify vouchers via fast ECDSA recovery instead of
	// an onchain isValidSignature (EIP-1271) call.
	if voucherKey := os.Getenv("EVM_VOUCHER_SIGNER_PRIVATE_KEY"); voucherKey != "" {
		voucherSigner, err := evmsigners.NewClientSignerFromPrivateKey(voucherKey)
		if err != nil {
			fmt.Printf("Failed to create voucher signer: %v\n", err)
			os.Exit(1)
		}
		cfg.VoucherSigner = voucherSigner
	}

	// Persist channel state so the subscription survives across client runs.
	if storageDir != "" {
		cfg.Storage = batchedclient.NewFileClientChannelStorage(batchsettlement.FileChannelStorageOptions{
			Directory: storageDir,
		})
	}

	scheme := batchedclient.NewBatchSettlementEvmScheme(signer, cfg)

	x402Client := x402.Newx402Client()
	x402Client.Register("eip155:*", scheme)

	httpClient := x402http.WrapHTTPClientWithPayment(http.DefaultClient, x402http.Newx402HTTPClient(x402Client))

	fmt.Printf("Subscriber:       %s\n", signer.Address())
	if cfg.VoucherSigner != nil {
		fmt.Printf("Voucher signer:   %s\n", cfg.VoucherSigner.Address())
	}
	fmt.Printf("Target:           %s\n", url)
	fmt.Printf("Requests:         %d  (1st opens the channel, rest are off-chain vouchers)\n\n", numberOfRequests)

	// resyncedOnce bounds channel-state auto-recovery to one attempt per run so
	// a genuinely broken channel cannot loop forever.
	resyncedOnce := false

	for i := 0; i < numberOfRequests; i++ {
		t0 := time.Now()

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := httpClient.Do(req)
		cancel()
		if err != nil {
			fmt.Printf("Request %d failed: %v\n", i+1, err)
			os.Exit(1)
		}

		label := "voucher"
		if i == 0 {
			label = "deposit + voucher"
		}
		fmt.Printf("Request %d [%s] — %s (%.3fs)\n", i+1, label, resp.Status, time.Since(t0).Seconds())

		if body, errBody := readJSON(resp); errBody == nil {
			fmt.Printf("  response: %s\n", compact(body))
		}
		if settle, _ := extractSettleResponse(resp); settle != nil {
			fmt.Printf("  settled:  %s\n", compact(settle))
		} else if resp.StatusCode != http.StatusOK {
			paymentRequired := extractPaymentRequired(resp)
			reason := "(no PAYMENT-REQUIRED header)"
			if paymentRequired != nil && paymentRequired.Error != "" {
				reason = paymentRequired.Error
			}
			fmt.Printf("  payment did not settle (%s) — reason: %s\n", resp.Status, reason)

			// The SDK auto-recovers cumulative mismatches, but a cooperative
			// refund bumps the onchain refundNonce and drains the balance —
			// state the SDK's corrective 402 path does not cover. Resync the
			// local session from chain and replay this request once.
			if !resyncedOnce && paymentRequired != nil && isChannelStateError(paymentRequired.Error) {
				if accept := findBatchedAccept(paymentRequired.Accepts); accept != nil {
					recCtx, recCancel := context.WithTimeout(context.Background(), 30*time.Second)
					_, recErr := scheme.RecoverSession(recCtx, *accept)
					recCancel()
					if recErr != nil {
						fmt.Printf("  [recover] onchain resync failed: %v\n", recErr)
					} else {
						resyncedOnce = true
						fmt.Println("  [recover] channel state resynced from chain — retrying request")
						_ = resp.Body.Close()
						fmt.Println()
						i--
						continue
					}
				}
			}
		}
		_ = resp.Body.Close()
		fmt.Println()
	}

	if refundAfterRequests {
		if refundAmount != "" {
			fmt.Printf("CANCEL — requesting partial refund of %s base units\n", refundAmount)
		} else {
			fmt.Println("CANCEL — requesting full refund of the remaining prepaid balance")
		}
		opts := &batchedclient.RefundOptions{}
		if refundAmount != "" {
			opts.Amount = refundAmount
		}
		refundCtx, refundCancel := context.WithTimeout(context.Background(), 60*time.Second)
		settle, err := scheme.Refund(refundCtx, url, opts)
		refundCancel()
		if err != nil {
			fmt.Printf("Refund failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("  refunded: %s\n", compact(settle))
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoiOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func readJSON(resp *http.Response) (interface{}, error) {
	var out interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func extractSettleResponse(resp *http.Response) (*x402.SettleResponse, error) {
	header := resp.Header.Get("PAYMENT-RESPONSE")
	if header == "" {
		header = resp.Header.Get("X-PAYMENT-RESPONSE")
	}
	if header == "" {
		return nil, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return nil, err
	}
	var out x402.SettleResponse
	if err := json.Unmarshal(decoded, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func compact(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// extractPaymentRequired decodes the v2 PAYMENT-REQUIRED header from a 402 so
// the demo can show WHY a payment was rejected and drive recovery.
func extractPaymentRequired(resp *http.Response) *x402.PaymentRequired {
	header := resp.Header.Get("PAYMENT-REQUIRED")
	if header == "" {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return nil
	}
	var out x402.PaymentRequired
	if err := json.Unmarshal(decoded, &out); err != nil {
		return nil
	}
	return &out
}

// isChannelStateError reports whether a 402 reason indicates the local channel
// session has diverged from onchain state (e.g. after a cooperative refund
// bumped the refundNonce) and an onchain resync could fix it.
func isChannelStateError(reason string) bool {
	return strings.Contains(reason, "batch_settlement")
}

func findBatchedAccept(accepts []types.PaymentRequirements) *types.PaymentRequirements {
	for i := range accepts {
		if accepts[i].Scheme == batchsettlement.SchemeBatched {
			return &accepts[i]
		}
	}
	return nil
}
