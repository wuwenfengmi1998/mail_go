package outbound

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	"github.com/emersion/go-msgauth/dkim"
)

// SignDKIM signs a raw RFC 5322 message with the domain's DKIM private key.
// When the private key is empty the message is returned unchanged (unsigned).
// The signature covers the standard header fields present in the message.
func SignDKIM(raw []byte, domainName, selector, privateKeyPEM string) ([]byte, error) {
	if privateKeyPEM == "" {
		return raw, nil
	}
	if domainName == "" {
		return raw, fmt.Errorf("DKIM domain is empty")
	}
	if selector == "" {
		selector = "default"
	}

	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return raw, fmt.Errorf("invalid DKIM private key PEM for %s", domainName)
	}

	signer, err := parseSigner(block)
	if err != nil {
		return raw, fmt.Errorf("parse DKIM private key for %s: %v", domainName, err)
	}

	var out bytes.Buffer
	options := &dkim.SignOptions{
		Domain:   domainName,
		Selector: selector,
		Signer:   signer,
	}
	if err := dkim.Sign(&out, bytes.NewReader(raw), options); err != nil {
		return raw, fmt.Errorf("DKIM signing for %s failed: %v", domainName, err)
	}
	return out.Bytes(), nil
}

// parseSigner parses an RSA private key PEM block into a crypto.Signer.
// Both PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE KEY") are supported.
func parseSigner(block *pem.Block) (crypto.Signer, error) {
	// PKCS#1 is what the domain management form stores.
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	// PKCS#8 fallback for externally generated keys.
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("unsupported private key type %T", parsed)
	}
	return signer, nil
}
