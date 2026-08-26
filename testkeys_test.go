package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"
)

// generateServiceAccountKey builds a syntactically real service-account key
// around a freshly generated RSA key. Keys are generated per test run and never
// committed — a private key in the repository is a private key in every fork.
func generateServiceAccountKey(t *testing.T, clientEmail string) string {
	t.Helper()
	// 2048 is the smallest size Go's JWT signing accepts without complaint and
	// keeps the test fast.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	data, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"project_id":   "rollout-test",
		"client_email": clientEmail,
		"client_id":    "1234567890",
		"private_key":  string(pemBytes),
		"token_uri":    "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatalf("marshal key JSON: %v", err)
	}
	return string(data)
}
