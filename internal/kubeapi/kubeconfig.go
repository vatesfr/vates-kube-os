package kubeapi

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Client talks to one API server with one identity.
type Client struct {
	http   *http.Client
	Server string
}

// The kubeconfig file, as far as this program reads it.
//
// Both ways a kubeconfig can carry credentials are supported, because both are
// in use in this project and a reader that understood only one would fail on
// half the files: kubeadm's admin.conf embeds the material as
// client-certificate-data, while the kubelet.conf this system installs points at
// the files under /etc/kubernetes/pki with client-certificate.
type kubeconfig struct {
	CurrentContext string            `yaml:"current-context"`
	Clusters       []namedCluster    `yaml:"clusters"`
	Users          []namedUser       `yaml:"users"`
	Contexts       []namedContextRef `yaml:"contexts"`
}

type namedCluster struct {
	Name    string `yaml:"name"`
	Cluster struct {
		Server                   string `yaml:"server"`
		CertificateAuthority     string `yaml:"certificate-authority"`
		CertificateAuthorityData string `yaml:"certificate-authority-data"`
	} `yaml:"cluster"`
}

type namedUser struct {
	Name string `yaml:"name"`
	User struct {
		ClientCertificate     string `yaml:"client-certificate"`
		ClientCertificateData string `yaml:"client-certificate-data"`
		ClientKey             string `yaml:"client-key"`
		ClientKeyData         string `yaml:"client-key-data"`
	} `yaml:"user"`
}

type namedContextRef struct {
	Name    string `yaml:"name"`
	Context struct {
		Cluster string `yaml:"cluster"`
		User    string `yaml:"user"`
	} `yaml:"context"`
}

// NewFromKubeconfig builds a client from a kubeconfig file on disk.
func NewFromKubeconfig(path string) (*Client, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var kc kubeconfig
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	// The current context names the cluster and user to use. A node's
	// kubeconfig has exactly one of each, so a missing or dangling context is
	// resolved by taking the only entry rather than refused.
	clusterName, userName := "", ""
	for _, c := range kc.Contexts {
		if c.Name == kc.CurrentContext {
			clusterName, userName = c.Context.Cluster, c.Context.User
		}
	}
	cluster, ok := pickCluster(kc.Clusters, clusterName)
	if !ok {
		return nil, fmt.Errorf("%s has no cluster this client could use", path)
	}
	user, ok := pickUser(kc.Users, userName)
	if !ok {
		return nil, fmt.Errorf("%s has no user this client could use", path)
	}

	if cluster.Cluster.Server == "" {
		return nil, fmt.Errorf("%s names no server", path)
	}

	certData := material(user.User.ClientCertificateData, user.User.ClientCertificate)
	keyData := material(user.User.ClientKeyData, user.User.ClientKey)
	if len(certData) == 0 || len(keyData) == 0 {
		return nil, fmt.Errorf("%s has no client certificate: neither client-certificate-data nor a readable client-certificate", path)
	}

	cert, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		return nil, fmt.Errorf("client certificate in %s: %w", path, err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	caData := material(cluster.Cluster.CertificateAuthorityData, cluster.Cluster.CertificateAuthority)
	if len(caData) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caData) {
			return nil, fmt.Errorf("the certificate authority in %s is not a PEM certificate", path)
		}
		tlsConfig.RootCAs = pool
	}

	return &Client{
		Server: cluster.Cluster.Server,
		http: &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsConfig},
		},
	}, nil
}

// pickCluster returns the named cluster, or the only one there is.
func pickCluster(all []namedCluster, name string) (namedCluster, bool) {
	for _, c := range all {
		if c.Name == name {
			return c, true
		}
	}
	if len(all) == 1 {
		return all[0], true
	}
	return namedCluster{}, false
}

// pickUser returns the named user, or the only one there is.
func pickUser(all []namedUser, name string) (namedUser, bool) {
	for _, u := range all {
		if u.Name == name {
			return u, true
		}
	}
	if len(all) == 1 {
		return all[0], true
	}
	return namedUser{}, false
}

// material returns the credential, from the file when the value is a path and
// from the value itself when it is embedded.
//
// A value that fails to read as a file is returned as bytes rather than
// rejected: in a hand-written kubeconfig the same field can hold raw PEM, and
// refusing it would mean refusing a file that curl would accept.
func material(data, file string) []byte {
	if data != "" {
		return mustBase64(data)
	}
	if file == "" {
		return nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	return raw
}

// mustBase64 decodes the base64 a kubeconfig uses for embedded material. A
// value that is already raw PEM is returned as it is, because the two are
// indistinguishable to a reader and only one of them is valid base64 of a
// certificate.
func mustBase64(s string) []byte {
	if decoded, err := base64.StdEncoding.DecodeString(s); err == nil && len(decoded) > 0 {
		return decoded
	}
	return []byte(s)
}
