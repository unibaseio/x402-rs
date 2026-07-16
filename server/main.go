// Command server is a resource server that sells a "subscription-like" metered
// API using the x402 batch-settlement scheme.
//
// How the subscription works:
//   - A subscriber opens a payment channel with a single onchain deposit (their
//     prepaid balance). This happens automatically on their first request.
//   - Every subsequent request is paid with an off-chain cumulative voucher —
//     no transaction, no gas, sub-second latency. This is the "metered usage".
//   - The ChannelManager periodically CLAIMS accumulated vouchers and SETTLES
//     the funds onchain to the receiver in batches, amortizing gas across many
//     requests instead of paying per call.
//   - When a subscriber cancels, a cooperative REFUND returns their unused
//     prepaid balance.
//
// This is the "deposit once, meter continuously, settle in batches" model that
// makes x402 feel like a prepaid subscription rather than pay-per-call.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	nethttpmw "github.com/x402-foundation/x402/go/v2/http/nethttp"
	batchsettlement "github.com/x402-foundation/x402/go/v2/mechanisms/evm/batch-settlement"
	batchedserver "github.com/x402-foundation/x402/go/v2/mechanisms/evm/batch-settlement/server"
)

const (
	defaultPort = "4021"
	network     = x402.Network("eip155:84532")

	// maxPricePerCall is the ceiling a subscriber authorizes per request. The
	// handler bills a fraction of this per call (metered usage), so subscribers
	// only pay for what they actually consume.
	maxPricePerCall = "$0.01"
)

// waitForFacilitator polls GET {url}/supported until the facilitator responds
// 200 or the timeout elapses, so server startup order doesn't matter.
func waitForFacilitator(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 5 * time.Second}
	for attempt := 1; ; attempt++ {
		resp, err := client.Get(url + "/supported")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("unexpected status %s", resp.Status)
		}
		if time.Now().After(deadline) {
			return err
		}
		if attempt == 1 {
			fmt.Printf("Waiting for facilitator at %s (%v)…\n", url, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func main() {
	_ = godotenv.Load()

	evmAddress := os.Getenv("EVM_ADDRESS")
	if !regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`).MatchString(evmAddress) {
		fmt.Println("Missing or invalid EVM_ADDRESS (checksummed 20-byte hex, 0x-prefixed)")
		os.Exit(1)
	}

	facilitatorURL := os.Getenv("FACILITATOR_URL")
	if facilitatorURL == "" {
		fmt.Println("Missing required FACILITATOR_URL environment variable")
		os.Exit(1)
	}

	// withdrawDelay is the challenge window (seconds) before the receiver can
	// force-withdraw a channel. Choose it larger than your claim cadence plus a
	// safety margin. Default 1 day.
	withdrawDelay := 86400
	if v := os.Getenv("DEFERRED_WITHDRAW_DELAY_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			withdrawDelay = n
		}
	}

	receiverAuthKey := os.Getenv("EVM_RECEIVER_AUTHORIZER_PRIVATE_KEY")
	storageDir := os.Getenv("STORAGE_DIR")

	cfg := &batchedserver.BatchSettlementEvmSchemeServerConfig{
		WithdrawDelay: withdrawDelay,
	}
	// Self-managed authorizer (recommended): channels survive a facilitator swap.
	if receiverAuthKey != "" {
		signer, err := newReceiverAuthorizerSigner(receiverAuthKey)
		if err != nil {
			fmt.Printf("Invalid EVM_RECEIVER_AUTHORIZER_PRIVATE_KEY: %v\n", err)
			os.Exit(1)
		}
		cfg.ReceiverAuthorizerSigner = signer
	}
	// Persist channel sessions so subscriptions survive restarts.
	if storageDir != "" {
		cfg.Storage = batchedserver.NewFileChannelStorage(batchsettlement.FileChannelStorageOptions{
			Directory: storageDir,
		})
	}

	scheme := batchedserver.NewBatchSettlementEvmScheme(evmAddress, cfg)

	// The middleware pulls /supported from the facilitator once at startup; if
	// that fails it stays permanently degraded (every request 402s). Block here
	// until the facilitator is actually reachable.
	if err := waitForFacilitator(facilitatorURL, 60*time.Second); err != nil {
		fmt.Printf("Facilitator not reachable at %s: %v\n", facilitatorURL, err)
		os.Exit(1)
	}

	facilitator := x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{
		URL: facilitatorURL,
		// Settlement waits for onchain confirmation (~15s on Base Sepolia, more
		// under load). The default 30s regularly trips on claim/settle batches.
		Timeout: 120 * time.Second,
	})

	// The ChannelManager is the subscription "biller": it batches claims,
	// settles funds to the receiver, and refunds idle/cancelled channels.
	manager := scheme.CreateChannelManager(facilitator, network)
	manager.Start(batchedserver.AutoSettlementConfig{
		ClaimIntervalSecs:  60,  // fold new vouchers into an onchain claim every 60s
		SettleIntervalSecs: 120, // sweep claimed funds to the receiver every 120s
		RefundIntervalSecs: 180, // scan for refundable (idle) channels every 180s
		MaxClaimsPerBatch:  100,
		// Refund a subscriber's unused balance after 3 minutes of inactivity
		// (demo cadence). In production, drive refunds off an explicit "cancel
		// subscription" signal instead of pure idleness.
		SelectRefundChannels: func(channels []*batchedserver.ChannelSession, ctx batchedserver.AutoSettlementContext) ([]*batchedserver.ChannelSession, error) {
			out := make([]*batchedserver.ChannelSession, 0, len(channels))
			for _, c := range channels {
				if c.Balance == "" || c.Balance == "0" {
					continue
				}
				if c.PendingRequest != nil && c.PendingRequest.ExpiresAt > ctx.Now {
					continue
				}
				if ctx.Now-c.LastRequestTimestamp < 180_000 {
					continue
				}
				out = append(out, c)
			}
			return out, nil
		},
		OnClaim: func(r batchedserver.ClaimResult) {
			fmt.Printf("[claim]  %d vouchers folded onchain (tx: %s)\n", r.Vouchers, r.Transaction)
		},
		OnSettle: func(r batchedserver.SettleResult) {
			fmt.Printf("[settle] swept to %s (tx: %s)\n", evmAddress, r.Transaction)
		},
		OnRefund: func(r batchedserver.RefundResult) {
			fmt.Printf("[refund] channel %s unused balance returned (tx: %s)\n", r.Channel, r.Transaction)
		},
		OnError: func(err error) {
			fmt.Printf("[biller-error] %v\n", err)
		},
	})

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)

	// Route config: the metered subscription endpoint. Price is the per-call
	// ceiling; the handler charges a fraction of it via Settlement-Overrides.
	routes := x402http.RoutesConfig{
		"GET /v1/insights": {
			Accepts: x402http.PaymentOptions{
				{
					Scheme:  batchsettlement.SchemeBatched,
					Price:   maxPricePerCall,
					Network: network,
					PayTo:   evmAddress,
				},
			},
			Description: "Metered Pro insights API (billed per call against your prepaid channel)",
			MimeType:    "application/json",
		},
	}

	mux := http.NewServeMux()

	// Free, unmetered endpoint — no payment required.
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "service": "unibase-pro"})
	})

	// Metered subscription endpoint. Each call bills 1–100% of maxPricePerCall
	// to demonstrate usage-based metering on top of the prepaid channel.
	mux.HandleFunc("GET /v1/insights", func(w http.ResponseWriter, r *http.Request) {
		usedPercent := 1 + rand.Intn(100)
		nethttpmw.SetSettlementOverrides(w, &x402.SettlementOverrides{
			Amount: fmt.Sprintf("%d%%", usedPercent),
		})

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"insight":     "Q3 signups up 18% WoW; churn concentrated in the free tier.",
			"tokens_used": usedPercent * 10,
			"billed_pct":  usedPercent,
		})
	})

	handler := nethttpmw.X402Payment(nethttpmw.Config{
		Routes:      routes,
		Facilitator: facilitator,
		Schemes: []nethttpmw.SchemeConfig{
			{Network: network, Server: scheme},
		},
		// Bounds the whole verify → handler → settle chain per request. The
		// first request of a subscription settles a deposit onchain, so give
		// it room beyond the ~15s Base Sepolia confirmation time.
		Timeout: 60 * time.Second,
	})(mux)

	httpServer := &http.Server{Addr: ":" + defaultPort, Handler: handler}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Server error: %v\n", err)
			os.Exit(1)
		}
	}()

	fmt.Printf("Unibase Pro subscription API listening at http://localhost:%s\n", defaultPort)
	fmt.Printf("  GET /v1/health   (free)\n")
	fmt.Printf("  GET /v1/insights (metered, up to %s/call)\n", maxPricePerCall)
	if cfg.ReceiverAuthorizerSigner != nil {
		fmt.Printf("  Receiver authorizer: self-managed %s\n", cfg.ReceiverAuthorizerSigner.Address())
	} else {
		fmt.Println("  Receiver authorizer: delegated to facilitator")
	}

	<-sigCh

	fmt.Println("Shutting down — flushing pending claims…")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = manager.Stop(ctx, &batchedserver.StopOptions{Flush: true})
	_ = httpServer.Shutdown(ctx)
}
