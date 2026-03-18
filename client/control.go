package client

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"time"

	"github.com/djylb/nps/lib/common"
	"github.com/djylb/nps/lib/conn"
	"github.com/djylb/nps/lib/crypt"
	"github.com/djylb/nps/lib/logs"
	"github.com/djylb/nps/lib/version"
)

const MaxPad = 64

// Ver 是客户端对外声明的协议版本索引。
var Ver = version.GetLatestIndex()

// SkipTLSVerify 控制 TLS 指纹校验是否跳过。
var SkipTLSVerify = false

func init() {
	crypt.InitTls(tls.Certificate{})
}

// VerifyState 校验 TLS 状态并返回证书指纹与系统校验结果。
func VerifyState(state tls.ConnectionState, host string) (fingerprint []byte, verified bool) {
	if len(state.PeerCertificates) == 0 {
		return nil, false
	}
	leaf := state.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, cert := range state.PeerCertificates[1:] {
		inter.AddCert(cert)
	}
	roots, _ := x509.SystemCertPool()
	opts := x509.VerifyOptions{
		DNSName:       host,
		Roots:         roots,
		Intermediates: inter,
	}
	if _, err := leaf.Verify(opts); err != nil {
		verified = false
	} else {
		verified = true
	}
	sum := sha256.Sum256(leaf.Raw)
	return sum[:], verified
}

// VerifyTLS 对已建立连接执行 TLS 握手并读取证书状态。
func VerifyTLS(connection net.Conn, host string) (fingerprint []byte, verified bool) {
	var tlsConn *tls.Conn
	if tc, ok := connection.(*conn.TlsConn); ok {
		tlsConn = tc.Conn
	} else if std, ok := connection.(*tls.Conn); ok {
		tlsConn = std
	} else {
		return nil, false
	}
	if err := tlsConn.Handshake(); err != nil {
		return nil, false
	}
	return VerifyState(tlsConn.ConnectionState(), host)
}

// EnsurePort 为服务端地址补齐默认端口。
func EnsurePort(server string, tp string) string {
	_, port, err := net.SplitHostPort(server)
	if err == nil && port != "" {
		return server
	}
	if p, ok := common.DefaultPort[tp]; ok {
		return net.JoinHostPort(server, p)
	}
	return server
}

// NewConn 创建到服务端的新连接，并完成版本/密钥握手。
// 这里只保留 tcp/tls 两种到服务端的连接方式，确保线协议兼容而不再引入 UDP 家族依赖。
func NewConn(tp string, vkey string, server string) (*conn.Conn, string, error) {
	var (
		err        error
		connection net.Conn
		isTls      bool
		tlsVerify  bool
		tlsFp      []byte
	)

	timeout := 10 * time.Second
	server, _ = common.SplitServerAndPath(server)
	addr, _, sni := common.SplitAddrAndHost(server)
	server = EnsurePort(addr, tp)

	switch tp {
	case "tcp":
		connection, err = net.DialTimeout("tcp", server, timeout)
	case "tls":
		isTls = true
		conf := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         sni,
		}
		rawConn, err := net.DialTimeout("tcp", server, timeout)
		if err != nil {
			return nil, "", err
		}
		connection, err = conn.NewTlsConn(rawConn, timeout, conf)
		if err != nil {
			_ = rawConn.Close()
			return nil, "", err
		}
		tlsFp, tlsVerify = VerifyTLS(connection, sni)
	default:
		return nil, "", fmt.Errorf("unsupported connection type %q, only tcp/tls are allowed", tp)
	}
	if err != nil {
		if connection != nil {
			_ = connection.Close()
		}
		return nil, "", err
	}

	_ = connection.SetDeadline(time.Now().Add(timeout))
	defer func() { _ = connection.SetDeadline(time.Time{}) }()

	c := conn.NewConn(connection)
	if _, err := c.BufferWrite([]byte(common.CONN_TEST)); err != nil {
		_ = c.Close()
		return nil, "", err
	}
	minVerBytes := []byte(version.GetVersion(Ver))
	if err := c.WriteLenContent(minVerBytes); err != nil {
		_ = c.Close()
		return nil, "", err
	}
	vs := []byte(version.VERSION)
	padLen := rand.Intn(MaxPad)
	if padLen > 0 {
		vs = append(vs, make([]byte, padLen)...)
	}
	if err := c.WriteLenContent(vs); err != nil {
		_ = c.Close()
		return nil, "", err
	}

	var uuid string
	if Ver == 0 {
		b, err := c.GetShortContent(32)
		if err != nil {
			logs.Error("%v", err)
			_ = c.Close()
			return nil, "", err
		}
		if crypt.Md5(version.GetVersion(Ver)) != string(b) {
			logs.Warn("客户端核心版本与服务端不匹配，当前核心版本为 %s", version.GetVersion(Ver))
		}
		if _, err := c.BufferWrite([]byte(crypt.Md5(vkey))); err != nil {
			_ = c.Close()
			return nil, "", err
		}
		if s, err := c.ReadFlag(); err != nil {
			_ = c.Close()
			return nil, "", err
		} else if s == common.VERIFY_EER {
			_ = c.Close()
			return nil, "", fmt.Errorf("validation key %s incorrect", vkey)
		}
	} else {
		ts := common.TimeNow().Unix() - int64(rand.Intn(6))
		if _, err := c.BufferWrite(common.TimestampToBytes(ts)); err != nil {
			_ = c.Close()
			return nil, "", err
		}
		if _, err := c.BufferWrite([]byte(crypt.Blake2b(vkey))); err != nil {
			_ = c.Close()
			return nil, "", err
		}

		var infoBuf []byte
		if Ver < 3 {
			infoBuf, err = crypt.EncryptBytes(common.EncodeIP(common.GetOutboundIP()), vkey)
			if err != nil {
				_ = c.Close()
				return nil, "", err
			}
		} else {
			ipPart := common.EncodeIP(common.GetOutboundIP())
			tpBytes := []byte(tp)
			tpLen := len(tpBytes)
			if tpLen > 32 {
				_ = c.Close()
				return nil, "", fmt.Errorf("tp too long: %d bytes (max %d)", tpLen, 32)
			}
			buf := make([]byte, 0, len(ipPart)+1+len(tpBytes))
			buf = append(buf, ipPart...)
			buf = append(buf, byte(tpLen))
			buf = append(buf, tpBytes...)
			infoBuf, err = crypt.EncryptBytes(buf, vkey)
			if err != nil {
				_ = c.Close()
				return nil, "", err
			}
		}
		if err := c.WriteLenContent(infoBuf); err != nil {
			_ = c.Close()
			return nil, "", err
		}
		randBuf, err := common.RandomBytes(1000)
		if err != nil {
			_ = c.Close()
			return nil, "", err
		}
		if err := c.WriteLenContent(randBuf); err != nil {
			_ = c.Close()
			return nil, "", err
		}
		hmacBuf := crypt.ComputeHMAC(vkey, ts, minVerBytes, vs, infoBuf, randBuf)
		if _, err := c.BufferWrite(hmacBuf); err != nil {
			_ = c.Close()
			return nil, "", err
		}
		b, err := c.GetShortContent(32)
		if err != nil {
			logs.Error("读取服务端响应失败: %v", err)
			_ = c.Close()
			return nil, "", fmt.Errorf("validation key %s incorrect", vkey)
		}
		if !bytes.Equal(b, crypt.ComputeHMAC(vkey, ts, hmacBuf, []byte(version.GetVersion(Ver)))) {
			logs.Warn("客户端核心版本与服务端不匹配，当前核心版本为 %s", version.GetVersion(Ver))
			_ = c.Close()
			return nil, "", fmt.Errorf("the client does not match the server version %s", version.GetVersion(Ver))
		}
		if Ver > 1 {
			fpBuf, err := c.GetShortLenContent()
			if err != nil {
				_ = c.Close()
				return nil, "", err
			}
			fpDec, err := crypt.DecryptBytes(fpBuf, vkey)
			if err != nil {
				_ = c.Close()
				return nil, "", err
			}
			if !SkipTLSVerify && isTls && !tlsVerify && !bytes.Equal(fpDec, tlsFp) {
				logs.Warn("证书校验失败，如需跳过请设置 -skip_verify=true")
				_ = c.Close()
				return nil, "", errors.New("validation cert incorrect")
			}
			crypt.AddTrustedCert(vkey, fpDec)
			if Ver > 3 {
				if Ver > 5 {
					uuidBuf, err := c.GetShortLenContent()
					if err != nil {
						_ = c.Close()
						return nil, "", err
					}
					uuid = string(uuidBuf)
				}
				if _, err := c.GetShortLenContent(); err != nil {
					_ = c.Close()
					return nil, "", err
				}
			}
		}
	}
	return c, uuid, nil
}

// SendType 向服务端声明当前连接用途，并保持原始线协议兼容。
func SendType(c *conn.Conn, connType, uuid string) error {
	if c == nil {
		return fmt.Errorf("sendType: nil conn (connType=%s uuid=%s)", connType, uuid)
	}
	if _, err := c.BufferWrite([]byte(connType)); err != nil {
		_ = c.Close()
		return err
	}
	if Ver > 3 {
		if Ver > 5 {
			if err := c.WriteLenContent([]byte(uuid)); err != nil {
				_ = c.Close()
				return err
			}
		}
		randByte, err := common.RandomBytes(1000)
		if err != nil {
			_ = c.Close()
			return err
		}
		if err := c.WriteLenContent(randByte); err != nil {
			_ = c.Close()
			return err
		}
	}
	if err := c.FlushBuf(); err != nil {
		_ = c.Close()
		return err
	}
	c.SetAlive()
	return nil
}
