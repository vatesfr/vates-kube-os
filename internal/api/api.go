package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/vatesfr/vates-kube-os/internal/firstboot"
	apiv1 "github.com/vatesfr/vates-kube-os/proto/vates/api/v1"
)

// DefaultPort is the management API port. 50000 is the immutable-OS convention;
// it is a default, not a rule -- api.port in vates-node.yaml overrides it.
const DefaultPort = 50000

// ClusterCAPath is the certificate authority kubeadm writes on a control plane.
// A worker receives the same CA through its config drive, so every node holds
// it and it can verify every client certificate the cluster ever issued.
const ClusterCAPath = "/etc/kubernetes/pki/ca.crt"

// ClusterCAKeyPath is the CA's private key, which only a control plane has. It
// signs the API's own server certificate when one has to be minted.
const ClusterCAKeyPath = "/etc/kubernetes/pki/ca.key"

// DefaultCertPath and DefaultKeyPath are where the server certificate is kept.
const (
	DefaultCertPath = "/var/lib/vates/api/tls.crt"
	DefaultKeyPath  = "/var/lib/vates/api/tls.key"
)

// Options configure a server. Every field has a working default on a control
// plane, so vates-api.service passes none of them.
type Options struct {
	// JoinMaterial produces fresh credentials for a machine that joins this
	// cluster. Empty means this node cannot issue any: they are signed or
	// encrypted with the cluster CA, which only a bootstrapped control plane
	// holds, so a worker -- and a control plane that has not bootstrapped --
	// leaves this nil and the method fails cleanly.
	JoinMaterial func(context.Context) (JoinMaterial, error)
	// Listen is host:port. Empty means every interface on DefaultPort.
	Listen string
	// CAPath is the authority that signs the server certificate and verifies
	// client certificates. Empty means the operator CA when both its files are
	// present, else the cluster CA.
	CAPath string
	// CAKeyPath is that authority's private key, used to mint the server
	// certificate. Empty follows the same rule as CAPath.
	CAKeyPath string
	// CertPath and KeyPath are the server's own certificate and key. Empty
	// means DefaultCertPath and DefaultKeyPath.
	CertPath string
	KeyPath  string
}

// JoinMaterial is what a joining machine needs from the cluster: a bootstrap
// token, the certificate key a joining control plane fetches the shared
// certificates with, and the CA hash it verifies the cluster with.
type JoinMaterial struct {
	Token          string
	CertificateKey string
	CACertHash     string
}

func (o *Options) applyDefaults() {
	if o.Listen == "" {
		o.Listen = fmt.Sprintf(":%d", DefaultPort)
	}
	// The operator CA, when BOTH its files were injected, replaces the cluster CA
	// entirely: it must sign the server certificate as well as verify clients, or
	// mutual TLS cannot complete. One file without the other is ignored, rather
	// than leaving the API with a certificate no client can verify.
	if o.CAPath == "" && o.CAKeyPath == "" && fileExists(firstboot.OperatorCAPath) && fileExists(firstboot.OperatorCAKeyPath) {
		o.CAPath, o.CAKeyPath = firstboot.OperatorCAPath, firstboot.OperatorCAKeyPath
	}
	if o.CAPath == "" {
		o.CAPath = ClusterCAPath
	}
	if o.CAKeyPath == "" {
		o.CAKeyPath = ClusterCAKeyPath
	}
	if o.CertPath == "" {
		o.CertPath = DefaultCertPath
	}
	if o.KeyPath == "" {
		o.KeyPath = DefaultKeyPath
	}
}

// Serve runs the management API until the process is stopped.
func Serve(opts Options) error {
	opts.applyDefaults()
	lis, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	return ServeListener(lis, opts)
}

// ServeListener serves on an already-open listener. It is separate from Serve so
// that tests can bind an ephemeral port.
func ServeListener(lis net.Listener, opts Options) error {
	opts.applyDefaults()
	if !fileExists(opts.CertPath) || !fileExists(opts.KeyPath) {
		if err := EnsureServerCert(opts.CAPath, opts.CAKeyPath, opts.CertPath, opts.KeyPath, serverHosts()); err != nil {
			return err
		}
	}
	serverOpt, err := serverOption(&opts)
	if err != nil {
		return err
	}
	srv := grpc.NewServer(serverOpt, grpc.UnaryInterceptor(authorize))
	apiv1.RegisterVatesAPIServer(srv, &service{joinMaterial: opts.JoinMaterial})
	return srv.Serve(lis)
}

// serverOption builds the mutual-TLS server credentials: the cluster CA verifies
// the CALLER, and the server presents its own certificate.
func serverOption(opts *Options) (grpc.ServerOption, error) {
	caPEM, err := os.ReadFile(opts.CAPath)
	if err != nil {
		return nil, fmt.Errorf("api: reading the cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("api: %s holds no certificate", opts.CAPath)
	}
	serverCert, err := tls.LoadX509KeyPair(opts.CertPath, opts.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("api: loading the server certificate: %w", err)
	}

	// RequireAndVerifyClientCert is the whole authentication: the cluster CA must
	// have signed the peer's certificate. The narrower group restriction lives in
	// authorize().
	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	})
	return grpc.Creds(creds), nil
}
