package outbound

import (
	"strings"
	"testing"

	dkimgen "mail_go/internal/dkim"

	msgauthdkim "github.com/emersion/go-msgauth/dkim"
)

func TestSignDKIMAndVerify(t *testing.T) {
	privPEM, pubPEM, err := dkimgen.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	_ = pubPEM

	raw := []byte("From: kevin@lmve.net\r\nTo: someone@example.com\r\nSubject: hello\r\n\r\nbody\r\n")

	signed, err := SignDKIM(raw, "lmve.net", "default", privPEM)
	if err != nil {
		t.Fatalf("SignDKIM: %v", err)
	}
	if !strings.Contains(string(signed), "DKIM-Signature") {
		t.Fatalf("signed message missing DKIM-Signature header")
	}

	verifications, err := msgauthdkim.Verify(strings.NewReader(string(signed)))
	if err != nil {
		t.Fatalf("dkim.Verify: %v", err)
	}
	if len(verifications) != 1 {
		t.Fatalf("expected 1 signature, got %d", len(verifications))
	}
	if verifications[0].Domain != "lmve.net" {
		t.Fatalf("unexpected signature: %+v", verifications[0])
	}
}

func TestSignDKIMEmptyKeyReturnsUnsigned(t *testing.T) {
	raw := []byte("From: kevin@lmve.net\r\nTo: someone@example.com\r\nSubject: hello\r\n\r\nbody\r\n")
	out, err := SignDKIM(raw, "lmve.net", "default", "")
	if err != nil {
		t.Fatalf("SignDKIM with empty key should not fail: %v", err)
	}
	if string(out) != string(raw) {
		t.Fatalf("message changed despite empty key")
	}
}
