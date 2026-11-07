package agent

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// CertReloader 为 HTTPS 入口提供可热更新的证书（A-W9、R-13）。
//
// 证书经 tls.Config.GetCertificate 提供，每次握手读取当前证书，所以续期不需要重建 GOST 服务，
// 存量连接也不受影响。检查按文件内容的摘要判断变化；新文件解析失败（格式错误、证书与私钥
// 不匹配、只写了一半）时保留旧证书并记录错误，下一次检查重试。证书续期由外部（ACME 客户端等）
// 负责写文件，这里只负责加载。
type CertReloader struct {
	certFile, keyFile string

	cur     atomic.Pointer[tls.Certificate]
	mu      sync.Mutex // 串行化 Reload
	digest  [sha256.Size]byte
	lastErr atomic.Pointer[error]
}

// NewCertReloader 立即加载一次证书；失败时返回错误（启动时缺证书应直接失败）。
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile}
	if _, err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload 检查文件是否变化，变化则加载。返回是否换上了新证书；解析失败时保留旧证书并返回错误。
func (r *CertReloader) Reload() (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	certPEM, err := os.ReadFile(r.certFile)
	if err != nil {
		return false, r.fail(err)
	}
	keyPEM, err := os.ReadFile(r.keyFile)
	if err != nil {
		return false, r.fail(err)
	}
	h := sha256.New()
	h.Write(certPEM)
	h.Write([]byte{0})
	h.Write(keyPEM)
	var d [sha256.Size]byte
	h.Sum(d[:0])
	if r.cur.Load() != nil && bytes.Equal(d[:], r.digest[:]) {
		return false, nil
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false, r.fail(fmt.Errorf("agent: load certificate %s: %w", r.certFile, err))
	}
	if cert.Leaf == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return false, r.fail(err)
		}
	}
	r.cur.Store(&cert)
	r.digest = d
	r.lastErr.Store(nil)
	return true, nil
}

func (r *CertReloader) fail(err error) error {
	r.lastErr.Store(&err)
	return err
}

// GetCertificate 实现 tls.Config.GetCertificate。
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.cur.Load(), nil
}

// TLSConfig 返回使用本加载器的服务端配置，可直接传给 Install。
func (r *CertReloader) TLSConfig() *tls.Config {
	return &tls.Config{GetCertificate: r.GetCertificate, MinVersion: tls.VersionTLS12}
}

// Leaf 返回当前证书，供证书到期告警使用。
func (r *CertReloader) Leaf() *x509.Certificate {
	if c := r.cur.Load(); c != nil {
		return c.Leaf
	}
	return nil
}

// LastError 返回最近一次检查的错误；最近一次成功或无变化时为 nil。
func (r *CertReloader) LastError() error {
	if p := r.lastErr.Load(); p != nil {
		return *p
	}
	return nil
}

// Watch 每 interval 检查一次文件，返回停止函数（可重复调用）。
func (r *CertReloader) Watch(interval time.Duration) (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				r.Reload()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done); <-exited })
	}
}
