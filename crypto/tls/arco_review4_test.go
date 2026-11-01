// Copyright 2026 The arco-nbio Authors.

// Review 4 regression tests (docs/risks.md §14).

package tls

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A server buffers at most maxPostHandshakeBuffered bytes of post-handshake
// handshake data; a larger message is never legitimate for a server.
func TestReview4PostHandshakeBufferLimit(t *testing.T) {
	for _, vers := range []uint16{VersionTLS12, VersionTLS13} {
		s := startNonBlockServer(t, nbServerConfig(vers), 4096, nil)
		raw, err := net.Dial("tcp", s.ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		cli := Client(raw, &Config{InsecureSkipVerify: true, MinVersion: vers, MaxVersion: vers})
		if _, err := cli.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		<-s.ready
		n := 8 << 10
		msg := make([]byte, 4+n/2) // a KeyUpdate header claiming n bytes, half sent
		msg[0], msg[1], msg[2], msg[3] = typeKeyUpdate, byte(n>>16), byte(n>>8), byte(n)
		cli.out.Lock()
		_, _ = cli.writeRecordLocked(recordTypeHandshake, msg)
		cli.out.Unlock()
		select {
		case err := <-s.done:
			if !errors.Is(err, errPostHandshakeTooLarge) {
				t.Fatalf("%s: server ended with %v, want errPostHandshakeTooLarge", VersionName(vers), err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: server kept the connection open", VersionName(vers))
		}
		raw.Close()
	}
}

// A recovered handshake panic skipped Go's c.handshakeErr assignment, so a
// later Write on another goroutine re-ran the handshake there, outside the
// recover, on input already buffered in c.rawInput (review 4).
func TestReview4PanicRecordsHandshakeErr(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	go func() { _, _ = io.Copy(io.Discard, b) }()
	var calls atomic.Int32
	cfg := testConfigServer.Clone()
	cfg.GetConfigForClient = func(*ClientHelloInfo) (*Config, error) {
		if calls.Add(1) == 1 {
			panic("boom once")
		}
		return nil, nil
	}
	srv := NewConn(a, cfg, false, true)
	hello := clientHelloBytes(t)
	two := append(append([]byte{}, hello...), hello...) // a pipelined second ClientHello
	if _, _, err := srv.AppendAndRead(two, make([]byte, 64)); err == nil || !strings.Contains(err.Error(), "panic during handshake") {
		t.Fatalf("AppendAndRead: %v", err)
	}
	if _, err := srv.Write([]byte("hi")); err == nil || !strings.Contains(err.Error(), "panic during handshake") {
		t.Fatalf("Write after the panic: %v, want the recorded panic error", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("GetConfigForClient called %d times: the handshake ran again", n)
	}
}
