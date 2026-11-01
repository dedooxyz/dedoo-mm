package chain

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
)

func testPub(t *testing.T, n int) string {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(priv.PubKey().SerializeCompressed())
}

func TestBuildMultisigRedeemScript(t *testing.T) {
	pubs := []string{testPub(t, 1), testPub(t, 2), testPub(t, 3)}

	twoOf3, err := BuildMultisigRedeemScript(pubs, []int{0, 1, 2}, 2)
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := BuildMultisigRedeemScript(pubs, []int{2, 1, 0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if twoOf3 == reordered {
		t.Fatal("expected distinct redeem scripts for distinct pubkey order")
	}

	// The threshold is env-driven (REQUIRED_SIGS), not baked in.
	oneOf3, err := BuildMultisigRedeemScript(pubs, []int{0, 1, 2}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := ParseMultisigThreshold(oneOf3); err != nil || m != 1 {
		t.Fatalf("1-of-3 threshold = %d, %v", m, err)
	}
	threeOf3, err := BuildMultisigRedeemScript(pubs, []int{0, 1, 2}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := ParseMultisigThreshold(threeOf3); err != nil || m != 3 {
		t.Fatalf("3-of-3 threshold = %d, %v", m, err)
	}
}

func TestBuildMultisigRedeemScriptErrors(t *testing.T) {
	pubs := []string{"aa", "bb", "cc"}
	if _, err := BuildMultisigRedeemScript(pubs, []int{0, 1}, 2); err == nil {
		t.Fatal("expected error for short order")
	}
	if _, err := BuildMultisigRedeemScript(pubs, []int{0, 1, 1}, 2); err == nil {
		t.Fatal("expected error for repeated index")
	}
	if _, err := BuildMultisigRedeemScript(pubs, []int{0, 1, 2}, 4); err == nil {
		t.Fatal("expected error for threshold > 3")
	}
}
