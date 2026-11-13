package client

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
)

// startTLSFDBServer serves the FDB ConnectPacket exchange behind TLS with cert,
// requiring a client certificate from clientCAs when it is non-nil.
func startTLSFDBServer(t *testing.T, cert tls.Certificate, clientCAs *x509.CertPool) string {
	t.Helper()
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	if clientCAs != nil {
		cfg.ClientCAs, cfg.ClientAuth = clientCAs, tls.RequireAndVerifyClientCert
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var buf [transport.ConnectPacketSize]byte
				if _, err := io.ReadFull(c, buf[:]); err != nil {
					return
				}
				sp := transport.ConnectPacket{ProtocolVersion: transport.ProtocolVersion73, ConnectionID: 1}
				if _, err := c.Write(sp.Marshal()); err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()
	return ln.Addr().String()
}
