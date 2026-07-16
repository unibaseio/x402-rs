// Throwaway e2e: prove the local facilitator handles the `upto` scheme —
// client authorizes up to $0.01 via Permit2, server charges 40% of it via
// Settlement-Overrides, facilitator verifies + settles the actual amount.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	nethttpmw "github.com/x402-foundation/x402/go/v2/http/nethttp"
	uptoclient "github.com/x402-foundation/x402/go/v2/mechanisms/evm/upto/client"
	uptoserver "github.com/x402-foundation/x402/go/v2/mechanisms/evm/upto/server"
	evmsigners "github.com/x402-foundation/x402/go/v2/signers/evm"
)

const network = x402.Network("eip155:84532")

func main() {
	payerKey := os.Getenv("PAYER_KEY")
	receiver := os.Getenv("RECEIVER")
	facilitatorURL := os.Getenv("FACILITATOR_URL")

	// --- server on :4027, upto scheme, authorize up to $0.01, charge 40% ---
	fac := x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{URL: facilitatorURL, Timeout: 120 * time.Second})
	routes := x402http.RoutesConfig{
		"GET /metered": {
			Accepts: x402http.PaymentOptions{
				{Scheme: "upto", Price: "$0.01", Network: network, PayTo: receiver},
			},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metered", func(w http.ResponseWriter, r *http.Request) {
		nethttpmw.SetSettlementOverrides(w, &x402.SettlementOverrides{Amount: "40%"})
		_ = json.NewEncoder(w).Encode(map[string]string{"pong": "upto", "billed": "40%"})
	})
	handler := nethttpmw.X402Payment(nethttpmw.Config{
		Routes:      routes,
		Facilitator: fac,
		Schemes:     []nethttpmw.SchemeConfig{{Network: network, Server: uptoserver.NewUptoEvmScheme()}},
		Timeout:     90 * time.Second,
	})(mux)
	srv := &http.Server{Addr: ":4027", Handler: handler}
	go func() { _ = srv.ListenAndServe() }()
	time.Sleep(500 * time.Millisecond)

	// --- client pays with upto (Permit2; allowance pre-approved) ---
	ethClient, err := ethclient.Dial("https://sepolia.base.org")
	if err != nil {
		fmt.Println("rpc:", err)
		os.Exit(1)
	}
	signer, err := evmsigners.NewClientSignerFromPrivateKeyWithClient(payerKey, ethClient)
	if err != nil {
		fmt.Println("signer:", err)
		os.Exit(1)
	}
	xc := x402.Newx402Client()
	xc.Register("eip155:*", uptoclient.NewUptoEvmScheme(signer, nil))
	httpClient := x402http.WrapHTTPClientWithPayment(http.DefaultClient, x402http.Newx402HTTPClient(xc))

	t0 := time.Now()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://localhost:4027/metered", nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Println("request:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	var body any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	fmt.Printf("upto payment: %s (%.1fs) body=%v\n", resp.Status, time.Since(t0).Seconds(), body)

	if h := resp.Header.Get("PAYMENT-RESPONSE"); h != "" {
		if raw, err := base64.StdEncoding.DecodeString(h); err == nil {
			fmt.Printf("settle: %s\n", raw)
		}
	} else {
		fmt.Println("no PAYMENT-RESPONSE — settlement did not happen")
		if h := resp.Header.Get("PAYMENT-REQUIRED"); h != "" {
			if raw, err := base64.StdEncoding.DecodeString(h); err == nil {
				fmt.Printf("payment-required: %s\n", raw)
			}
		}
		os.Exit(1)
	}
}
