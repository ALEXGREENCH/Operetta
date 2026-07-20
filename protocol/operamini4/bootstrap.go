// Package operamini4 implements the transport used by Opera Mini 4.x.
//
// Opera Mini 4 does not speak the NUL-separated request/OMS response protocol
// used by the earlier clients. Its first exchange establishes a server RSA key
// and verifies it with a key baked into the MIDlet manifest.
package operamini4

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
	"time"
)

// OriginUserAgent approximates the browser identity used by the Opera Mini 4
// transcoding service when it fetches origin pages. Keeping it separate from
// the encrypted client identity lets the HTML transformer request comparable
// mobile/fallback markup.
const OriginUserAgent = "Opera/9.80 (J2ME/MIDP; Opera Mini/4.2.15410/34.818; U; en) Presto/2.8.119 Version/11.10"

const (
	SigningModulusHex = "b957df6b4c85dda670b9e2ded531f2b08c8ce4c57aaa31edf107424cc635b9be6f634fb42eeb546ef18d137d3657c28571aaaec64158251bad253d7b9555252271ca361655479193c3df6531e795fea373d054186ff34bf37935de7de30a2030b333a1ea67da48f53dcfe8d022ec4b37f4d55106adf37ba2f46fe8dd00a16561576912c38d8b3a6551058ead7a3311d2ca3373f299134da1ef834be1a759b463"
	signingPrivateHex = "7b8fea4788593e6ef5d141e9e376a1cb085dedd8fc71769ea0af81888423d1299f978a781f478d9f4bb36253798fd703a11c74842b9018bd1e18d3a7b8e36e16f686ceb98e2fb6628294ee21450ea9c129e4f708bdcf8d3ac9f270217b3f1216f0b65692b795409d20b91f035459066aaf19475560fa3440c224556dfbcd546dbddc77ef965a4662ebc618886121762ac3dc503f0302241de0659bcf0b43debb"

	TransportModulusHex = "b2b4565218e92ba78713e96b34895e7d8d057fe28542de8f8b1b84216b95fbc534789c70417b22ce68282aacdfeeaa93e7bd923c42f1e42595ed80aa3442b9e2331fe9d28266a6105e0ad57a424d8e1b4c83edc98a653fb4aa2ed48c386a23cd0bd6a1114378140c494a43fa72766167077e005d6e98b9d12a3b87ba64fa43d7"
	transportPrivateHex = "7722e436bb461d1a5a0d4647785b9453b358ffec58d73f0a5cbd02c0f263fd2e22fb12f580fcc1def01ac71dea9f1c629a7e617d81f698190e9e55c6cd81d140599c4933dfe3f40d13df716b627eb5c81e643c4e26206aa4e4d7972928c751800530613b1420ffa6083c6e1bdab56dcf83354730daceb46aa62307a33433900b"

	signatureBytes = 160
	transportBytes = 128
)

var (
	signingModulus   = mustBig(SigningModulusHex)
	signingPrivate   = mustBig(signingPrivateHex)
	transportModulus = mustBig(TransportModulusHex)
	transportPrivate = mustBig(transportPrivateHex)
)

// IsBootstrapHello identifies the 11-byte unauthenticated OM4 greeting.
func IsBootstrapHello(payload []byte) bool {
	return len(payload) == 11 && payload[0] == 1 && payload[1] == 1 && payload[2] == 0
}

// BootstrapResponse returns the key-negotiation reply expected by OM4. The
// timestamp is moved slightly into the future because the client rejects a
// server timestamp that becomes older than its clock while the reply is read.
func BootstrapResponse(now time.Time, token []byte) []byte {
	if len(token) > 255 {
		panic("OM4 bootstrap token is too long")
	}
	timestamp := now.Add(5 * time.Minute).UnixMilli()
	modulus := fixedBytes(transportModulus, transportBytes)

	digestInput := make([]byte, 8+len(modulus))
	binary.BigEndian.PutUint64(digestInput, uint64(timestamp))
	copy(digestInput[8:], modulus)
	digest := sha256.Sum256(digestInput)

	encoded := make([]byte, signatureBytes)
	encoded[1] = 1
	for i := 2; i < signatureBytes-len(digest)-1; i++ {
		encoded[i] = 0xff
	}
	copy(encoded[signatureBytes-len(digest):], digest[:])
	signed := new(big.Int).Exp(new(big.Int).SetBytes(encoded), signingPrivate, signingModulus)
	signature := fixedBytes(signed, signatureBytes)

	// Byte 1 encodes (modulusBytes-128)/4. Byte 2 is the length of an
	// optional server token; a zero token is sufficient for a fresh session.
	response := make([]byte, 3+len(token)+signatureBytes+transportBytes+8)
	response[0] = 2
	response[1] = byte((transportBytes - 128) / 4)
	response[2] = byte(len(token))
	copy(response[3:], token)
	signatureOffset := 3 + len(token)
	copy(response[signatureOffset:], signature)
	copy(response[signatureOffset+signatureBytes:], modulus)
	binary.BigEndian.PutUint64(response[len(response)-8:], uint64(timestamp))
	return response
}

// DecryptTransportBlock applies the OM4 transport private key to an RSA block.
// The returned value is always modulus-sized so the PKCS#1-style envelope can
// be inspected without losing its leading zero byte.
func DecryptTransportBlock(ciphertext []byte) ([]byte, error) {
	value := new(big.Int).SetBytes(ciphertext)
	if value.Cmp(transportModulus) >= 0 {
		return nil, errors.New("OM4 RSA ciphertext exceeds transport modulus")
	}
	plain := new(big.Int).Exp(value, transportPrivate, transportModulus)
	return fixedBytes(plain, transportBytes), nil
}

func fixedBytes(value *big.Int, size int) []byte {
	raw := value.Bytes()
	if len(raw) > size {
		panic("OM4 integer does not fit its protocol field")
	}
	out := make([]byte, size)
	copy(out[size-len(raw):], raw)
	return out
}

func mustBig(value string) *big.Int {
	raw, err := hex.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return new(big.Int).SetBytes(raw)
}
