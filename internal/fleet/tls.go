package fleet

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"slices"
)

// TLSOptions describe one side of a mutual-TLS connection.
type TLSOptions struct {
	// CertFile and KeyFile (PEM) or Certificates give this side's
	// identity. A server always needs one; a client needs one for mTLS.
	CertFile, KeyFile string
	Certificates      []tls.Certificate
	// CAFile or CAPEM holds the CA bundle used to verify the peer: client
	// certificates on a server, the server certificate on a client.
	CAFile string
	CAPEM  []byte
	// AllowedNames, if set, restricts accepted peer certificates to those
	// whose subject common name, DNS SAN or URI SAN is listed (e.g.
	// "worker.fleet.internal" or "spiffe://corp/fleet/worker").
	AllowedNames []string
	// ServerName overrides the name a client verifies.
	ServerName string
	// MinVersion defaults to TLS 1.3 and cannot go below TLS 1.2.
	MinVersion uint16
}

func (o TLSOptions) certs() ([]tls.Certificate, error) {
	certs := slices.Clone(o.Certificates)
	if o.CertFile != "" || o.KeyFile != "" {
		c, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("fleet: tls key pair: %w", err)
		}
		certs = append(certs, c)
	}
	return certs, nil
}

func (o TLSOptions) pool() (*x509.CertPool, error) {
	pem := slices.Clone(o.CAPEM)
	if o.CAFile != "" {
		b, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("fleet: tls ca: %w", err)
		}
		pem = append(pem, b...)
	}
	if len(pem) == 0 {
		return nil, nil
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%w: no certificates in CA bundle", ErrInvalid)
	}
	return p, nil
}

// verifyNames returns a VerifyConnection hook enforcing AllowedNames on
// the verified peer leaf.
func (o TLSOptions) verifyNames() func(tls.ConnectionState) error {
	if len(o.AllowedNames) == 0 {
		return nil
	}
	allowed := slices.Clone(o.AllowedNames)
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("fleet: peer presented no certificate")
		}
		leaf := cs.PeerCertificates[0]
		names := append([]string{leaf.Subject.CommonName}, leaf.DNSNames...)
		for _, u := range leaf.URIs {
			names = append(names, u.String())
		}
		for _, n := range names {
			if n != "" && slices.Contains(allowed, n) {
				return nil
			}
		}
		return fmt.Errorf("fleet: peer certificate %q is not an allowed identity", leaf.Subject.CommonName)
	}
}

// ServerTLSConfig builds a server configuration that requires and verifies
// client certificates against the CA bundle (mutual TLS).
func ServerTLSConfig(o TLSOptions) (*tls.Config, error) {
	certs, err := o.certs()
	if err != nil {
		return nil, err
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("%w: server TLS needs a certificate", ErrInvalid)
	}
	pool, err := o.pool()
	if err != nil {
		return nil, err
	}
	if pool == nil {
		return nil, fmt.Errorf("%w: mutual TLS needs a client CA bundle", ErrInvalid)
	}
	return &tls.Config{
		Certificates:     certs,
		ClientAuth:       tls.RequireAndVerifyClientCert,
		ClientCAs:        pool,
		MinVersion:       minTLSVersion(o.MinVersion),
		VerifyConnection: o.verifyNames(),
	}, nil
}

// ClientTLSConfig builds a client configuration presenting a client
// certificate and verifying the server against the CA bundle (or the
// system roots when none is given).
func ClientTLSConfig(o TLSOptions) (*tls.Config, error) {
	certs, err := o.certs()
	if err != nil {
		return nil, err
	}
	pool, err := o.pool()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates:     certs,
		RootCAs:          pool,
		ServerName:       o.ServerName,
		MinVersion:       minTLSVersion(o.MinVersion),
		VerifyConnection: o.verifyNames(),
	}, nil
}

func minTLSVersion(v uint16) uint16 {
	if v == 0 {
		return tls.VersionTLS13
	}
	return max(v, tls.VersionTLS12)
}
