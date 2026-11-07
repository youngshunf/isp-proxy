package agentproto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// DevCertOptions 配置开发证书。
type DevCertOptions struct {
	// ServerHosts 是控制面服务端证书的 SAN（域名或 IP）；为空时用 localhost、127.0.0.1、::1。
	ServerHosts []string
	// NodeIDs 是要签发客户端证书的节点 ID。
	NodeIDs []string
	// Validity 是证书有效期，默认 1 年。
	Validity time.Duration
}

// CertKeyPaths 是一对证书与私钥文件。
type CertKeyPaths struct {
	Cert string
	Key  string
}

// DevCerts 是 GenerateDevCerts 写出的文件。
type DevCerts struct {
	CA     CertKeyPaths
	Server CertKeyPaths
	Nodes  map[string]CertKeyPaths
}

// GenerateDevCerts 在 dir 中生成开发用 CA、控制面服务端证书与各节点客户端证书（ECDSA P-256），
// 供 `isp-control dev-certs` 与测试使用。生成的文件不得进 Git（docs/06 §5）。
//
// 已存在的文件会被复用而不是覆盖：CA 存在时用它签发新增的节点证书，因此可重复执行以追加节点。
// 文件名：ca.pem、ca-key.pem、server.pem、server-key.pem、node-<id>.pem、node-<id>-key.pem；私钥权限 0600。
func GenerateDevCerts(dir string, opts DevCertOptions) (*DevCerts, error) {
	if opts.Validity <= 0 {
		opts.Validity = 365 * 24 * time.Hour
	}
	if len(opts.ServerHosts) == 0 {
		opts.ServerHosts = []string{"localhost", "127.0.0.1", "::1"}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	out := &DevCerts{
		CA:     CertKeyPaths{filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")},
		Server: CertKeyPaths{filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem")},
		Nodes:  map[string]CertKeyPaths{},
	}

	caCert, caKey, err := loadPair(out.CA)
	if errors.Is(err, os.ErrNotExist) {
		caCert, caKey, err = newCA(opts.Validity)
		if err == nil {
			err = writePair(out.CA, caCert, caKey)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("agentproto: CA: %w", err)
	}

	if !exists(out.Server.Cert) {
		tmpl := leafTemplate("isp-control", opts.Validity, x509.ExtKeyUsageServerAuth)
		for _, h := range opts.ServerHosts {
			if ip := net.ParseIP(h); ip != nil {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			} else {
				tmpl.DNSNames = append(tmpl.DNSNames, h)
			}
		}
		if err := issue(out.Server, tmpl, caCert, caKey); err != nil {
			return nil, fmt.Errorf("agentproto: 服务端证书: %w", err)
		}
	}

	for _, id := range opts.NodeIDs {
		if !nodeIDPattern.MatchString(id) {
			return nil, fmt.Errorf("agentproto: 节点 ID %q 格式不正确", id)
		}
		p := CertKeyPaths{filepath.Join(dir, "node-"+id+".pem"), filepath.Join(dir, "node-"+id+"-key.pem")}
		out.Nodes[id] = p
		if exists(p.Cert) {
			continue
		}
		tmpl := leafTemplate(NodeCommonName(id), opts.Validity, x509.ExtKeyUsageClientAuth)
		if err := issue(p, tmpl, caCert, caKey); err != nil {
			return nil, fmt.Errorf("agentproto: 节点 %s 证书: %w", id, err)
		}
	}
	return out, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err)
	}
	return n
}

func newCA(validity time.Duration) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "isp-proxy dev CA", Organization: []string{"isp-proxy (development only)"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

func leafTemplate(cn string, validity time.Duration, usage x509.ExtKeyUsage) *x509.Certificate {
	now := time.Now()
	return &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
}

func issue(p CertKeyPaths, tmpl, ca *x509.Certificate, caKey *ecdsa.PrivateKey) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	return writePair(p, cert, key)
}

func writePair(p CertKeyPaths, cert *x509.Certificate, key *ecdsa.PrivateKey) error {
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p.Key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(p.Cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644)
}

func loadPair(p CertKeyPaths) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, err := os.ReadFile(p.Cert)
	if err != nil {
		return nil, nil, err
	}
	kb, err := os.ReadFile(p.Key)
	if err != nil {
		return nil, nil, err
	}
	cblk, _ := pem.Decode(cb)
	kblk, _ := pem.Decode(kb)
	if cblk == nil || kblk == nil {
		return nil, nil, errors.New("PEM 无法解析")
	}
	cert, err := x509.ParseCertificate(cblk.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kblk.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}
