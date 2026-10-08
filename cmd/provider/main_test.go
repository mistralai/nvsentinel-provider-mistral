// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
)

func writeCertificate(t *testing.T, certPath, keyPath string, serial int64) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"provider.example.test"}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestTLSReloadsMountedCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeCertificate(t, certPath, keyPath, 1)
	cfg, err := rotatingTLS(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatal("TLS minimum changed")
	}
	for _, serial := range []int64{1, 2} {
		writeCertificate(t, certPath, keyPath, serial)
		pair, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil || cert.SerialNumber.Int64() != serial {
			t.Fatal("rotated certificate was not loaded")
		}
	}
	if err := os.WriteFile(keyPath, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.GetCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Fatal("invalid rotated key must fail closed")
	}
}

func TestRPCDeadlineIsBounded(t *testing.T) {
	for _, parentLimit := range []time.Duration{time.Second, 10 * time.Millisecond} {
		ctx, cancel := context.WithTimeout(context.Background(), parentLimit)
		defer cancel()
		parentDeadline, _ := ctx.Deadline()
		_, err := boundedRPC(100*time.Millisecond)(ctx, nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, _ any) (any, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 100*time.Millisecond {
				t.Fatal("RPC has no bounded deadline")
			}
			if deadline.After(parentDeadline) {
				t.Fatal("caller deadline was extended")
			}
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
