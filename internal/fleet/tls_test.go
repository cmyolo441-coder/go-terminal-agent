package fleet

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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fleet test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *testCA) issue(t *testing.T, cn string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		DNSNames: []string{cn}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestMutualTLS(t *testing.T) {
	ca := newTestCA(t)
	srvCfg, err := ServerTLSConfig(TLSOptions{
		Certificates: []tls.Certificate{ca.issue(t, "coordinator", x509.ExtKeyUsageServerAuth)},
		CAPEM:        ca.pem, AllowedNames: []string{"worker-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.TLS.PeerCertificates[0].Subject.CommonName))
	}))
	srv.TLS = srvCfg
	srv.StartTLS()
	defer srv.Close()

	get := func(cert *tls.Certificate) error {
		o := TLSOptions{CAPEM: ca.pem, ServerName: "coordinator"}
		if cert != nil {
			o.Certificates = []tls.Certificate{*cert}
		}
		cfg, err := ClientTLSConfig(o)
		if err != nil {
			return err
		}
		hc := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		resp, err := hc.Get(srv.URL)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
	good := ca.issue(t, "worker-1", x509.ExtKeyUsageClientAuth)
	if err := get(&good); err != nil {
		t.Fatalf("allowed client refused: %v", err)
	}
	if err := get(nil); err == nil {
		t.Fatal("client without a certificate accepted")
	}
	other := ca.issue(t, "worker-2", x509.ExtKeyUsageClientAuth)
	if err := get(&other); err == nil {
		t.Fatal("client outside AllowedNames accepted")
	}
	rogue := newTestCA(t).issue(t, "worker-1", x509.ExtKeyUsageClientAuth)
	if err := get(&rogue); err == nil {
		t.Fatal("client from another CA accepted")
	}
	if _, err := ServerTLSConfig(TLSOptions{CAPEM: ca.pem}); err == nil {
		t.Fatal("server config without a certificate")
	}
	if _, err := ServerTLSConfig(TLSOptions{Certificates: []tls.Certificate{good}}); err == nil {
		t.Fatal("mTLS server config without a client CA")
	}
}
