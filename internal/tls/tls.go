// Package tls builds a *tls.Config for edge TLS termination.
//
// Problem:  TLS at the edge must fail fast on missing/expired material and
//
//	never log key paths beyond what is needed to debug.
//
// Options:  (a) inline in cli, (b) small internal package.
// Choice:   (b) — keeps cli thin and makes cert validation unit-testable.
// Failure:  unreadable or mismatched cert/key → error before the listener binds.
// Observability: boot log records tls enabled + min_version only.
package tls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"time"
)

// Load validates certFile/keyFile and returns a *tls.Config with the
// requested minimum protocol version ("1.2" default, "1.3" supported).
// Expired or not-yet-valid certificates are rejected at startup.
func Load(certFile, keyFile, minVersion string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load tls key pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse tls certificate: %w", err)
	}
	now := time.Now()
	if now.After(leaf.NotAfter) {
		return nil, fmt.Errorf("tls certificate expired at %s", leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	if now.Before(leaf.NotBefore) {
		return nil, fmt.Errorf("tls certificate not valid before %s", leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	var ver uint16
	switch minVersion {
	case "", "1.2":
		ver = tls.VersionTLS12
	case "1.3":
		ver = tls.VersionTLS13
	default:
		return nil, fmt.Errorf("unsupported tls min_version %q", minVersion)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   ver,
	}, nil
}
