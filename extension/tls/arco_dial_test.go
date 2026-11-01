// Copyright 2026 The arco-nbio Authors.

package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	stdtls "crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

// Dial used crypto/tls's Dial, whose Conn is not made by NewConn: the first
// SetSession or ResetConn then replaced the Conn's transport, racing with
// LocalAddr, Close and other methods that read it without c.in/c.out (found in
// the upstream review of the nbio PR). Run with -race.
func TestDialSetSessionNoRace(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := stdtls.Listen("tcp", "127.0.0.1:0", &stdtls.Config{
		Certificates: []stdtls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 1)
				_, _ = c.Read(buf)
				c.Close()
			}()
		}
	}()

	c, err := Dial("tcp", ln.Addr().String(), &Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = c.LocalAddr()
		}
	}()
	c.SetSession("x")
	wg.Wait()
	if c.Session() != "x" {
		t.Fatal("session not stored")
	}
	if _, ok := c.Conn().(*net.TCPConn); !ok {
		t.Fatalf("Conn() = %T, want the TCP transport", c.Conn())
	}
}
