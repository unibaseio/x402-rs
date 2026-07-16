// Throwaway testnet helper: fund the payer wallet with gas ETH, then approve
// Permit2 on USDC from the payer. Base Sepolia only.
package main

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	rpcURL  = "https://sepolia.base.org"
	usdc    = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
	permit2 = "0x000000000022D473030F116dDEE9F6B43aC78BA3"
)

func must[T any](v T, err error) T {
	if err != nil {
		fmt.Println("fatal:", err)
		os.Exit(1)
	}
	return v
}

func sendTx(ctx context.Context, client *ethclient.Client, key *ecdsa.PrivateKey, to common.Address, value *big.Int, data []byte) string {
	from := crypto.PubkeyToAddress(key.PublicKey)
	nonce := must(client.PendingNonceAt(ctx, from))
	gasPrice := must(client.SuggestGasPrice(ctx))
	gas := uint64(21000)
	if len(data) > 0 {
		gas = 80000
	}
	tx := types.NewTransaction(nonce, to, value, gas, gasPrice, data)
	chainID := must(client.ChainID(ctx))
	signed := must(types.SignTx(tx, types.NewEIP155Signer(chainID), key))
	if err := client.SendTransaction(ctx, signed); err != nil {
		fmt.Println("send:", err)
		os.Exit(1)
	}
	for i := 0; i < 60; i++ {
		if r, err := client.TransactionReceipt(ctx, signed.Hash()); err == nil {
			if r.Status != 1 {
				fmt.Println("tx reverted:", signed.Hash().Hex())
				os.Exit(1)
			}
			return signed.Hash().Hex()
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Println("timeout waiting for", signed.Hash().Hex())
	os.Exit(1)
	return ""
}

func main() {
	ctx := context.Background()
	client := must(ethclient.Dial(rpcURL))
	funder := must(crypto.HexToECDSA(strings.TrimPrefix(os.Getenv("FUNDER_KEY"), "0x")))
	payer := must(crypto.HexToECDSA(strings.TrimPrefix(os.Getenv("PAYER_KEY"), "0x")))
	payerAddr := crypto.PubkeyToAddress(payer.PublicKey)

	// 1. 0.003 ETH funder → payer
	amount := big.NewInt(3_000_000_000_000_000)
	fmt.Println("fund tx:", sendTx(ctx, client, funder, payerAddr, amount, nil))

	// 2. payer: USDC.approve(permit2, 10 USDC)
	sel := crypto.Keccak256([]byte("approve(address,uint256)"))[:4]
	arg1 := common.LeftPadBytes(common.HexToAddress(permit2).Bytes(), 32)
	arg2 := common.LeftPadBytes(big.NewInt(10_000_000).Bytes(), 32)
	data := append(append(sel, arg1...), arg2...)
	fmt.Println("approve tx:", sendTx(ctx, client, payer, common.HexToAddress(usdc), big.NewInt(0), data))
}
