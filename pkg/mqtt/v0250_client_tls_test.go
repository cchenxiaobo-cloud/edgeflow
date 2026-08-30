package mqtt

// v0250_client_tls_test.go — TLS support tests for the MQTT client layer
// (v0.25.0). Everything is self-contained: self-signed certificates are
// generated in-process (crypto/ecdsa + crypto/x509), the fake broker is a
// bare crypto/tls listener speaking the same codec primitives as
// v0240_client_test.go's plaintext fakeBroker (which stays untouched).

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

// tlsTestCerts holds a generated self-signed CA plus a server certificate
// signed by that CA, with 127.0.0.1 (and ::1) as IP SANs.
type tlsTestCerts struct {
	caPEM      []byte // CA cert, PEM-encoded (RootCAs pool input)
	serverCert tls.Certificate
}

// generateTLSCerts creates a fresh CA and server certificate. Tests generate
// their own material so no fixtures or external tooling are needed.
func generateTLSCerts(t *testing.T) *tlsTestCerts {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "edgeflow-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create server cert: %v", err)
	}
	srvTLSCert := tls.Certificate{
		Certificate: [][]byte{srvDER, caDER},
		PrivateKey:  srvKey,
	}
	return &tlsTestCerts{caPEM: caPEM, serverCert: srvTLSCert}
}

// rootPool builds a x509 CertPool trusting only the generated CA.
func (c *tlsTestCerts) rootPool(t *testing.T) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(c.caPEM) {
		t.Fatal("AppendCertsFromPEM failed")
	}
	return pool
}

// tlsFakeBroker is the TLS variant of fakeBroker: a bare tls.Listener that
// answers CONNECT with CONNACK{0}, SUBACKs each Subscribe echoing the
// requested QoS, PUBACKs QoS1 publishes, records publishes and echoes each
// publish back to the client (for roundtrip testing).
type tlsFakeBroker struct {
	ln net.Listener

	mu    sync.Mutex
	pubs  []*Publish
	subs  []*Subscribe
	conns int
}

func startTLSBroker(t *testing.T, certs *tlsTestCerts) *tlsFakeBroker {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{certs.serverCert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	b := &tlsFakeBroker{ln: ln}
	go b.acceptLoop()
	return b
}

func (b *tlsFakeBroker) addr() string { return b.ln.Addr().String() }

func (b *tlsFakeBroker) acceptLoop() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.serve(conn)
	}
}

func (b *tlsFakeBroker) serve(conn net.Conn) {
	b.mu.Lock()
	b.conns++
	b.mu.Unlock()
	defer conn.Close()
	for {
		p, err := decodePacket(conn)
		if err != nil {
			return
		}
		switch pv := p.(type) {
		case *Connect:
			_ = encodePacket(conn, &Connack{ReturnCode: 0})
		case *Subscribe:
			b.mu.Lock()
			b.subs = append(b.subs, pv)
			b.mu.Unlock()
			codes := make([]byte, len(pv.Topics))
			for i, tf := range pv.Topics {
				codes[i] = tf.QoS
			}
			_ = encodePacket(conn, &Suback{PacketID: pv.PacketID, Codes: codes})
		case *Publish:
			b.mu.Lock()
			b.pubs = append(b.pubs, pv)
			b.mu.Unlock()
			if pv.QoS == 1 {
				_ = encodePacket(conn, &Puback{PacketID: pv.PacketID})
			}
			// Echo back so client-side subscriptions can roundtrip.
			_ = encodePacket(conn, &Publish{QoS: 0, Topic: pv.Topic, Payload: pv.Payload})
		case *Pingreq:
			_ = encodePacket(conn, &Pingresp{})
		case *Disconnect:
			return
		}
	}
}

func (b *tlsFakeBroker) recorded() (pubs []*Publish, subs []*Subscribe, conns int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	pubs = append(pubs, b.pubs...)
	subs = append(subs, b.subs...)
	return pubs, subs, b.conns
}

// TestV0250DialTLSHandshake: TLSConfig + trusted self-signed CA → TLS
// handshake, CONNECT/CONNACK(0) and a healthy usable client.
func TestV0250DialTLSHandshake(t *testing.T) {
	certs := generateTLSCerts(t)
	broker := startTLSBroker(t, certs)

	cl, err := Dial(broker.addr(), Options{
		ClientID:  "tls-hs",
		TLSConfig: &tls.Config{RootCAs: certs.rootPool(t)},
	})
	if err != nil {
		t.Fatalf("Dial over TLS: %v", err)
	}
	defer cl.Close()

	if _, _, conns := broker.recorded(); conns != 1 {
		t.Fatalf("broker conns = %d, want 1", conns)
	}
}

// TestV0250PublishSubscribeRoundtripOverTLS: full Publish/Subscribe
// roundtrip on a TLS connection, including a QoS1 publish and an inbound
// push echoed by the broker.
func TestV0250PublishSubscribeRoundtripOverTLS(t *testing.T) {
	certs := generateTLSCerts(t)
	broker := startTLSBroker(t, certs)

	cl, err := Dial(broker.addr(), Options{
		ClientID:  "tls-rw",
		KeepAlive: 30 * time.Second,
		TLSConfig: &tls.Config{RootCAs: certs.rootPool(t)},
	})
	if err != nil {
		t.Fatalf("Dial over TLS: %v", err)
	}
	defer cl.Close()

	got := make(chan string, 4)
	if err := cl.Subscribe("edge/tls/#", 1, func(topic string, payload []byte) {
		got <- string(payload)
	}); err != nil {
		t.Fatalf("Subscribe over TLS: %v", err)
	}
	if err := cl.Publish("edge/tls/data", 1, []byte("ping-over-tls")); err != nil {
		t.Fatalf("Publish over TLS: %v", err)
	}

	select {
	case payload := <-got:
		if payload != "ping-over-tls" {
			t.Fatalf("roundtrip payload = %q, want %q", payload, "ping-over-tls")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for publish roundtrip over TLS")
	}

	pubs, subs, _ := broker.recorded()
	if len(pubs) != 1 || pubs[0].Topic != "edge/tls/data" || string(pubs[0].Payload) != "ping-over-tls" {
		t.Fatalf("broker pubs = %+v, want one edge/tls/data publish", pubs)
	}
	if len(subs) != 1 || subs[0].Topics[0].Topic != "edge/tls/#" {
		t.Fatalf("broker subs = %+v, want edge/tls/#", subs)
	}
}

// TestV0250DialTLSWrongCA: an untrusting client (empty RootCAs) must fail
// the handshake with a certificate verification error.
func TestV0250DialTLSWrongCA(t *testing.T) {
	certs := generateTLSCerts(t)
	broker := startTLSBroker(t, certs)

	_, err := Dial(broker.addr(), Options{
		ClientID:  "tls-badca",
		TLSConfig: &tls.Config{RootCAs: x509.NewCertPool()},
	})
	if err == nil {
		t.Fatal("Dial with empty RootCAs: want error, got nil")
	}
}

// TestV0250DialTLSInsecureSkipVerify: InsecureSkipVerify=true skips cert
// validation, so the handshake succeeds against the self-signed broker.
func TestV0250DialTLSInsecureSkipVerify(t *testing.T) {
	certs := generateTLSCerts(t)
	broker := startTLSBroker(t, certs)

	cl, err := Dial(broker.addr(), Options{
		ClientID:  "tls-insecure",
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // intentional: exercises the skip-verify path
	})
	if err != nil {
		t.Fatalf("Dial with InsecureSkipVerify: %v", err)
	}
	defer cl.Close()

	// Connection must be usable: a QoS0 publish is fire-and-forget.
	if err := cl.Publish("edge/tls/insecure", 0, []byte("x")); err != nil {
		t.Fatalf("Publish on skip-verify connection: %v", err)
	}
}

// TestV0250DialTLSServerNameAutofill: addr host 127.0.0.1 with no explicit
// ServerName — Dial backfills ServerName from the address so verification
// succeeds against the IP SAN in the server certificate.
func TestV0250DialTLSServerNameAutofill(t *testing.T) {
	certs := generateTLSCerts(t)
	broker := startTLSBroker(t, certs)

	host, port, err := net.SplitHostPort(broker.addr())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	cfg := &tls.Config{RootCAs: certs.rootPool(t)}
	if cfg.ServerName != "" {
		t.Fatal("precondition: ServerName should be empty")
	}
	cl, err := Dial(net.JoinHostPort(host, port), Options{
		ClientID:  "tls-sni",
		TLSConfig: cfg,
	})
	if err != nil {
		t.Fatalf("Dial with autofilled ServerName: %v", err)
	}
	defer cl.Close()

	// The caller's config must not have been mutated by Dial (Clone).
	if cfg.ServerName != "" {
		t.Fatalf("caller TLSConfig.ServerName mutated to %q, want unchanged", cfg.ServerName)
	}
}
