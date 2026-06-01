package dkim

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
)

// GenerateKeyPair 生成 2048 位 RSA 密钥对，返回 PEM 编码的私钥和公钥
func GenerateKeyPair() (privateKeyPEM, publicKeyPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", fmt.Errorf("生成RSA密钥对失败: %w", err)
	}

	privBytes := x509.MarshalPKCS1PrivateKey(key)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privBytes})

	pubBytes, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", fmt.Errorf("编码公钥失败: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})

	return string(privPEM), string(pubPEM), nil
}

// GetDKIMDNSRecord 生成 DKIM DNS TXT 记录值
// 格式: v=DKIM1; k=rsa; p=<base64公钥>
func GetDKIMDNSRecord(publicKeyPEM string) string {
	if publicKeyPEM == "" {
		return ""
	}
	block, _ := pem.Decode([]byte(publicKeyPEM))
	if block == nil {
		return ""
	}
	return fmt.Sprintf("v=DKIM1; k=rsa; p=%s", base64.StdEncoding.EncodeToString(block.Bytes))
}
