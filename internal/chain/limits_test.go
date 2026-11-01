package chain

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
)

func TestMaxInputsForRedeem(t *testing.T) {
	pubs := []string{testPub(t, 1), testPub(t, 2), testPub(t, 3)}
	redeem, err := BuildMultisigRedeemScript(pubs, []int{0, 1, 2}, 2)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		required int
		min      int
		max      int
	}{
		{1, 420, 460},
		{2, 320, 345},
		{3, 255, 280},
	}
	for _, tc := range cases {
		n := MaxInputsForRedeem(redeem, tc.required)
		if n < tc.min || n > tc.max {
			t.Fatalf("%d-of-3 limit = %d, want %d..%d", tc.required, n, tc.min, tc.max)
		}
	}
	// More signatures per input must never allow more inputs.
	if MaxInputsForRedeem(redeem, 3) >= MaxInputsForRedeem(redeem, 2) {
		t.Fatal("3-of-3 limit must be lower than 2-of-3")
	}
}

func TestBuildAtChainInputLimit(t *testing.T) {
	priv1, _ := btcec.NewPrivateKey()
	priv2, _ := btcec.NewPrivateKey()
	priv3, _ := btcec.NewPrivateKey()
	pubs := []string{
		fmt.Sprintf("%x", priv1.PubKey().SerializeCompressed()),
		fmt.Sprintf("%x", priv2.PubKey().SerializeCompressed()),
		fmt.Sprintf("%x", priv3.PubKey().SerializeCompressed()),
	}
	redeem, err := BuildMultisigRedeemScript(pubs, []int{0, 1, 2}, 2)
	if err != nil {
		t.Fatal(err)
	}
	maxInputs := MaxInputsForRedeem(redeem, 2)

	utxos := make([]UTXO, maxInputs)
	for i := range utxos {
		utxos[i] = UTXO{
			Txid:          fmt.Sprintf("%064x", i+1),
			Vout:          0,
			Value:         1_000_000,
			Confirmations: 30,
		}
	}
	outputs := []Output{{Address: testP2SHOutput, ValueSats: 1_000_000}}
	raw, err := BuildAndSignMultisigTx(utxos, outputs, redeem, []*btcec.PrivateKey{priv1, priv2})
	if err != nil {
		t.Fatalf("build at the computed limit (%d inputs): %v", maxInputs, err)
	}
	if size := len(raw) / 2; size > MaxStandardTxBytes {
		t.Fatalf("transaction at limit is %d bytes, above %d", size, MaxStandardTxBytes)
	}
}
