package operamini4

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math/big"
	"testing"
	"time"
)

func TestBootstrapResponseVerifiesLikeClient(t *testing.T) {
	now := time.Unix(1_750_000_000, 0)
	token := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	response := BootstrapResponse(now, token)
	if len(response) != 307 {
		t.Fatalf("len(response)=%d, want 307", len(response))
	}
	if response[0] != 2 || response[1] != 0 || response[2] != 8 {
		t.Fatalf("unexpected bootstrap prefix: %x", response[:3])
	}

	signature := new(big.Int).SetBytes(response[11:171])
	decoded := fixedBytes(new(big.Int).Exp(signature, big.NewInt(3), signingModulus), signatureBytes)
	if decoded[0] != 0 || decoded[1] != 1 || decoded[127] != 0 {
		t.Fatalf("invalid signed envelope: %x", decoded[:8])
	}
	if !bytes.Equal(decoded[2:127], bytes.Repeat([]byte{0xff}, 125)) {
		t.Fatal("invalid signature padding")
	}

	timestamp := binary.BigEndian.Uint64(response[len(response)-8:])
	input := append([]byte(nil), response[len(response)-8:]...)
	input = append(input, response[171:299]...)
	want := sha256.Sum256(input)
	if !bytes.Equal(decoded[128:], want[:]) {
		t.Fatalf("signature digest mismatch: %x", decoded[128:])
	}
	if timestamp <= uint64(now.UnixMilli()) {
		t.Fatal("bootstrap timestamp is not in the future")
	}
}

func TestTransportRSADecrypt(t *testing.T) {
	plain := make([]byte, transportBytes)
	plain[1] = 2
	copy(plain[len(plain)-32:], bytes.Repeat([]byte{0x5a}, 32))
	cipher := new(big.Int).Exp(new(big.Int).SetBytes(plain), big.NewInt(3), transportModulus)
	got, err := DecryptTransportBlock(fixedBytes(cipher, transportBytes))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("RSA transport round trip mismatch")
	}
}
