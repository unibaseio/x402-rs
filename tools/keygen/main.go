// Command keygen prints fresh secp256k1 keypairs for local testing.
//
//	go run .           # one keypair
//	go run . 3         # three keypairs
//
// These are TEST keys. Never fund a key you printed to a terminal with real money.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/ethereum/go-ethereum/crypto"
)

func main() {
	n := 1
	if len(os.Args) > 1 {
		if v, err := strconv.Atoi(os.Args[1]); err == nil && v > 0 {
			n = v
		}
	}
	for i := 0; i < n; i++ {
		pk, err := crypto.GenerateKey()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		priv := fmt.Sprintf("0x%x", crypto.FromECDSA(pk))
		addr := crypto.PubkeyToAddress(pk.PublicKey).Hex()
		fmt.Printf("private_key=%s\naddress=%s\n\n", priv, addr)
	}
}
