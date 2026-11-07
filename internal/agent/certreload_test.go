package agent

// A-W9：HTTPS 入口证书加载与热更新（R-13）。

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
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pemPair 生成序列号为 serial 的自签证书与私钥（PEM）。
func pemPair(t *testing.T, serial int64) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "proxy.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Duration(serial) * 24 * time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// servedSerial 与 HTTPS 入口握手，返回对方证书序列号。
func servedSerial(t *testing.T, addr string) int64 {
	t.Helper()
	c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
}

func TestCertReloaderHotSwapsHTTPSEntry(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if _, err := NewCertReloader(certFile, keyFile); err == nil {
		t.Fatal("missing files accepted")
	}
	c1, k1 := pemPair(t, 1)
	writeFile(t, certFile, c1)
	writeFile(t, keyFile, k1)
	r, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}

	e := newEnv(t, time.Second)
	Install(e.a, r.TLSConfig())
	acc := e.provision("s1", 1<<40, time.Hour)
	if s := servedSerial(t, acc.HTTPSAddr); s != 1 {
		t.Fatalf("serving serial %d", s)
	}
	live, err := tunnel(pHTTPS, acc.HTTPSAddr, acc.Username, acc.Password, e.canary.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()

	if changed, err := r.Reload(); changed || err != nil {
		t.Fatalf("unchanged files: changed=%v err=%v", changed, err)
	}

	// 续期：新证书立即用于新握手，服务不重启，存量连接不受影响。
	c2, k2 := pemPair(t, 2)
	writeFile(t, certFile, c2)
	writeFile(t, keyFile, k2)
	if changed, err := r.Reload(); !changed || err != nil {
		t.Fatalf("renewal: changed=%v err=%v", changed, err)
	}
	if s := servedSerial(t, acc.HTTPSAddr); s != 2 {
		t.Fatalf("after renewal serving serial %d", s)
	}
	if r.Leaf().SerialNumber.Int64() != 2 || time.Until(r.Leaf().NotAfter) < 47*time.Hour {
		t.Fatal("Leaf does not describe the served certificate")
	}
	if body, err := get(live, "/ip"); err != nil || body != "exit-A" {
		t.Fatalf("existing HTTPS connection broken by reload: %q %v", body, err)
	}

	// 解析失败保留旧证书。
	writeFile(t, certFile, []byte("not a certificate"))
	if changed, err := r.Reload(); changed || err == nil {
		t.Fatalf("garbage: changed=%v err=%v", changed, err)
	}
	if r.LastError() == nil {
		t.Fatal("LastError not recorded")
	}
	if s := servedSerial(t, acc.HTTPSAddr); s != 2 {
		t.Fatalf("garbage replaced cert: serial %d", s)
	}

	// 只写了一半（新证书配旧私钥）同样保留旧证书；写完后下一次检查生效。
	c3, k3 := pemPair(t, 3)
	writeFile(t, certFile, c3)
	if _, err := r.Reload(); err == nil {
		t.Fatal("mismatched pair accepted")
	}
	writeFile(t, keyFile, k3)
	if changed, err := r.Reload(); !changed || err != nil {
		t.Fatalf("completed pair: changed=%v err=%v", changed, err)
	}
	if r.LastError() != nil {
		t.Fatal("LastError not cleared after success")
	}
	if s := servedSerial(t, acc.HTTPSAddr); s != 3 {
		t.Fatalf("serial %d, want 3", s)
	}

	// 后台轮询。
	stop := r.Watch(20 * time.Millisecond)
	defer stop()
	c4, k4 := pemPair(t, 4)
	writeFile(t, keyFile, k4)
	writeFile(t, certFile, c4)
	eventually(t, 2*time.Second, func() bool { return r.Leaf().SerialNumber.Int64() == 4 }, "watch did not pick up renewal")
	if s := servedSerial(t, acc.HTTPSAddr); s != 4 {
		t.Fatalf("serial %d, want 4", s)
	}
	stop()
	stop() // 可重复调用
}
