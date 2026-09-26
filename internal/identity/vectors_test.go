package identity

import (
	"crypto/ed25519"
	"encoding/hex"
	"testing"
)

// Test vectors published in docs/PROTOCOL.md ("Test vectors"). Native
// clients unit-test their pairing and auth code against the same values, so
// if this test has to change, the document must change with it.
const (
	vecCode        = "ABCD-EFGH"
	vecNormalized  = "ABCDEFGH"
	vecFingerprint = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	vecSeed        = "a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf"
	vecNonce       = "404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f"

	vecPublicKey   = "4fd099ccd47d7893dfe9ec24414ecb0d9b5420232aad30d91c465be33cbe65c4"
	vecDeviceID    = "a18112b0b7b4225ff30527e0f7cf7a1e"
	vecPairProof   = "69d5f9d7bb63848f45ed99cb5d05b3d77a4782452df8f1fbd8417de0d512aebf"
	vecServerProof = "a6bc3eaf7fd3f1df1f6929e408676c03ca15ddf2e8a4d445ff3cc9f72f55d0c3"
	vecAuthMessage = "666c6565742d617574682d7631" + vecNonce + vecFingerprint
	vecSignature   = "6ee459e68222c2c84442f826baad8ea9b87208b0284344ac4ca211e74bc1e382" +
		"8ebf12494e4908f9b3000864d72ee98df2e7c28689c4342585e10bf0ac4cdf0d"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestProtocolVectors(t *testing.T) {
	fp, nonce := unhex(t, vecFingerprint), unhex(t, vecNonce)
	dev := newDevice(ed25519.NewKeyFromSeed(unhex(t, vecSeed)))

	for _, in := range []string{vecCode, "abcd efgh", "AB-CD-EF-GH"} {
		if got := NormalizeCode(in); got != vecNormalized {
			t.Errorf("NormalizeCode(%q) = %q, want %q", in, got, vecNormalized)
		}
	}
	if got := NormalizeCode("0I1L-OOAB"); got != "011100AB" {
		t.Errorf("NormalizeCode look-alikes = %q, want 011100AB", got)
	}

	checks := []struct{ name, got, want string }{
		{"public key", hex.EncodeToString(dev.Public), vecPublicKey},
		{"device id", dev.ID, vecDeviceID},
		{"pair proof", hex.EncodeToString(PairProof(vecCode, fp, dev.Public)), vecPairProof},
		{"server proof", hex.EncodeToString(ServerPairProof(vecCode, fp, dev.Public)), vecServerProof},
		{"auth message", hex.EncodeToString(AuthMessage(nonce, fp)), vecAuthMessage},
		{"signature", hex.EncodeToString(dev.Sign(nonce, fp)), vecSignature},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, c.got, c.want)
		}
	}
	if !VerifyAuth(dev.Public, nonce, fp, unhex(t, vecSignature)) {
		t.Error("VerifyAuth rejects the published signature")
	}
}
