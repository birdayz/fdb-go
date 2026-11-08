package plugin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"golang.org/x/net/proxy"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/recordlayer"
)

// APIVersion is the FDB API version main selects once per process.
const APIVersion = 730

// fdbHandle is the instance's one FDB database, shared by SQL and OTel reads.
type fdbHandle struct {
	db    *recordlayer.FDBDatabase
	close func()
}

// fdbOpener opens the instance's database; tests substitute a fake.
type fdbOpener func(ctx context.Context, s *Settings, inst backend.DataSourceInstanceSettings) (*fdbHandle, error)

// openFDB opens the cluster named by the settings with the settings' TLS and
// PDC dialer. ctx bounds the coordinator bootstrap only.
func openFDB(ctx context.Context, s *Settings, inst backend.DataSourceInstanceSettings) (*fdbHandle, error) {
	version, err := fdb.GetAPIVersion()
	if err != nil {
		return nil, err
	}
	opts := []client.Option{client.WithAPIVersion(version)}
	tlsCfg, err := buildTLSConfig(s)
	if err != nil {
		return nil, err
	}
	if tlsCfg != nil {
		opts = append(opts, client.WithTLSConfig(tlsCfg))
	}
	if s.SecureSocksProxy {
		dial, err := pdcDialer(ctx, inst)
		if err != nil {
			return nil, err
		}
		opts = append(opts, client.WithDialFunc(dial))
	}

	var cf *client.ClusterFile
	if s.ClusterString != "" {
		cf, err = client.ParseClusterString(s.ClusterString)
	} else {
		cf, err = client.ParseClusterFile(s.ClusterFilePath)
	}
	if err != nil {
		return nil, err
	}
	if cf.UseTLS && s.TLSMode == tlsModeOff {
		return nil, settingsErrorf("tlsMode", "the cluster's coordinators use TLS (:tls) but tlsMode is off")
	}
	var db *client.Database
	if s.ClusterString != "" {
		// Coordinator changes stay in memory: there is no file to write back to.
		db, err = client.OpenDatabaseFromConfig(ctx, cf, opts...)
	} else {
		db, err = client.OpenDatabase(ctx, s.ClusterFilePath, opts...)
	}
	if err != nil {
		return nil, err
	}
	raw := fdb.WrapDatabase(db)
	if err := raw.Options().SetTransactionTimeout(s.TransactionTimeout.Milliseconds()); err != nil {
		raw.Close()
		return nil, err
	}
	return &fdbHandle{db: recordlayer.NewFDBDatabase(raw), close: raw.Close}, nil
}

// buildTLSConfig returns the explicit TLS configuration of tlsMode config, or
// nil for off and cluster (the client then resolves ":tls" + FDB_TLS_* itself).
func buildTLSConfig(s *Settings) (*tls.Config, error) {
	if s.TLSMode != tlsModeConfig {
		return nil, nil
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(s.TLSCACert)) {
		return nil, settingsErrorf("tlsCACert", "no PEM certificate found")
	}
	policy := transport.DefaultVerifyPolicy
	if s.TLSVerifyPeers != "" {
		p, err := transport.ParseVerifyPeers(s.TLSVerifyPeers)
		if err != nil {
			return nil, settingsErrorf("tlsVerifyPeers", "%v", err)
		}
		policy = p
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		// FDB certificates rarely carry IP SANs, so the host-name check is off and
		// the verify-peers policy checks the chain against the CA instead (libfdb_c).
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection below verifies the peer.
		VerifyConnection:   policy.VerifyConnection(roots),
	}
	if s.TLSClientCert != "" {
		cert, err := tls.X509KeyPair([]byte(s.TLSClientCert), []byte(s.TLSClientKey))
		if err != nil {
			return nil, settingsErrorf("tlsClientCert", "%v", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// errPDCUnavailable is returned when the datasource asks for the secure socks
// proxy but Grafana has none configured; connecting directly instead would
// bypass the network path the operator chose.
var errPDCUnavailable = errors.New("enableSecureSocksProxy is set but Grafana has no secure socks proxy configured")

func pdcDialer(ctx context.Context, inst backend.DataSourceInstanceSettings) (client.DialFunc, error) {
	pc, err := inst.ProxyClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("secure socks proxy: %w", err)
	}
	if !pc.SecureSocksProxyEnabled() {
		return nil, errPDCUnavailable
	}
	d, err := pc.NewSecureSocksProxyContextDialer()
	if err != nil {
		return nil, fmt.Errorf("secure socks proxy: %w", err)
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("secure socks proxy: dialer does not support contexts")
	}
	return cd.DialContext, nil
}
