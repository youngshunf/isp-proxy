package agentproto

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// NodeCNPrefix 是节点客户端证书 CN 的前缀：CN = "node:<id>"。
const NodeCNPrefix = "node:"

var nodeIDPattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// NodeCommonName 返回节点证书应使用的 CN。
func NodeCommonName(nodeID string) string { return NodeCNPrefix + nodeID }

// NodeIDFromCert 从已验证的客户端证书取节点 ID。
func NodeIDFromCert(cert *x509.Certificate) (string, error) {
	cn := cert.Subject.CommonName
	if !strings.HasPrefix(cn, NodeCNPrefix) {
		return "", fmt.Errorf("agentproto: 客户端证书 CN %q 不是 %q<id>", cn, NodeCNPrefix)
	}
	id := strings.TrimPrefix(cn, NodeCNPrefix)
	if !nodeIDPattern.MatchString(id) {
		return "", fmt.Errorf("agentproto: 节点 ID %q 格式不正确", id)
	}
	return id, nil
}

// ErrNoClientCert 表示请求没有经过验证的客户端证书。
var ErrNoClientCert = errors.New("agentproto: 缺少已验证的客户端证书")

// NodeIDFromRequest 从 mTLS 请求取节点 ID。只信任 TLS 层已验证的证书链，不读任何请求头。
func NodeIDFromRequest(r *http.Request) (string, error) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return "", ErrNoClientCert
	}
	return NodeIDFromCert(r.TLS.VerifiedChains[0][0])
}

func caPool(caPEM []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("agentproto: CA 证书不是有效的 PEM")
	}
	return pool, nil
}

// ServerTLSConfig 构造控制面节点接口（:8443）的 TLS 配置：TLS 1.3，强制并验证由 caPEM 签发的客户端证书。
func ServerTLSConfig(caPEM, certPEM, keyPEM []byte) (*tls.Config, error) {
	pool, err := caPool(caPEM)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("agentproto: 服务端证书: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}, nil
}

// ClientTLSConfig 构造 agent 侧 TLS 配置：用节点证书做客户端认证，只信任 caPEM 签发的服务端证书。
// serverName 为空时使用连接地址中的主机名。
func ClientTLSConfig(caPEM, certPEM, keyPEM []byte, serverName string) (*tls.Config, error) {
	pool, err := caPool(caPEM)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("agentproto: 节点证书: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
	}, nil
}

func readAll(files ...string) ([][]byte, error) {
	out := make([][]byte, len(files))
	for i, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("agentproto: %w", err)
		}
		out[i] = b
	}
	return out, nil
}

// LoadServerTLSConfig 从文件读取 CA、服务端证书与私钥，见 ServerTLSConfig。
func LoadServerTLSConfig(caFile, certFile, keyFile string) (*tls.Config, error) {
	b, err := readAll(caFile, certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return ServerTLSConfig(b[0], b[1], b[2])
}

// LoadClientTLSConfig 从文件读取 CA、节点证书与私钥，见 ClientTLSConfig。
func LoadClientTLSConfig(caFile, certFile, keyFile, serverName string) (*tls.Config, error) {
	b, err := readAll(caFile, certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return ClientTLSConfig(b[0], b[1], b[2], serverName)
}
