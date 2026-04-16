package conn

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// AdbCtlMessage 是服务端发送给 NPC 客户端的 ADB 控制消息。
// 通过 mux 隧道传输，NPC 收到后在本地执行 adbd 的 stop/start/restart 操作。
// 协议格式：[4字节消息长度][JSON消息体]
type AdbCtlMessage struct {
	Command string `json:"command"` // 控制命令：start/stop/restart
}

// AdbCtlResponse 是 NPC 客户端执行 ADB 控制命令后返回的结果。
type AdbCtlResponse struct {
	Success bool   `json:"success"` // 是否执行成功
	Message string `json:"message"` // 执行结果或错误信息
}

// WriteAdbCtlMessage 将 ADBCTL 消息按 [4字节长度][JSON] 格式写入连接。
// 协议格式与 LinkInfo 保持一致：先写 4 字节小端序的消息体长度，再写 JSON 内容。
func WriteAdbCtlMessage(w io.Writer, msg *AdbCtlMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("序列化 ADBCTL 消息失败: %w", err)
	}
	// 先写 4 字节长度头（小端序），与 GetLinkInfo 的读取方式兼容
	if err := binary.Write(w, binary.LittleEndian, int32(len(data))); err != nil {
		return fmt.Errorf("写入 ADBCTL 消息长度失败: %w", err)
	}
	_, err = w.Write(data)
	return err
}

// ReadAdbCtlMessage 从连接读取 ADBCTL 响应。
// 协议格式：[4字节长度][JSON响应体]
func ReadAdbCtlMessage(r io.Reader) (*AdbCtlResponse, error) {
	var msgLen int32
	if err := binary.Read(r, binary.LittleEndian, &msgLen); err != nil {
		return nil, fmt.Errorf("读取 ADBCTL 响应长度失败: %w", err)
	}
	if msgLen <= 0 || msgLen > 4096 {
		return nil, fmt.Errorf("ADBCTL 响应长度异常: %d", msgLen)
	}
	buf := make([]byte, msgLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("读取 ADBCTL 响应内容失败: %w", err)
	}
	var resp AdbCtlResponse
	if err := json.Unmarshal(buf, &resp); err != nil {
		return nil, fmt.Errorf("解析 ADBCTL 响应失败: %w", err)
	}
	return &resp, nil
}

type Secret struct {
	//Type     string // tcp/udp
	Password string
	Conn     *Conn
	//Tunnel   *mux.Mux
}

func NewSecret(p string, conn *Conn) *Secret {
	return &Secret{
		//Type:     tp,
		Password: p,
		Conn:     conn,
		//Tunnel:   tunnel,
	}
}

type Link struct {
	ConnType   string //连接类型
	Host       string //目标
	Crypt      bool   //加密
	Compress   bool
	LocalProxy bool
	RemoteAddr string
	Option     Options
}

type Option func(*Options)

type Options struct {
	Timeout time.Duration
	NeedAck bool
}

var defaultTimeOut = time.Second * 5

func NewLink(connType string, host string, crypt bool, compress bool, remoteAddr string, localProxy bool, opts ...Option) *Link {
	options := newOptions(opts...)

	return &Link{
		RemoteAddr: remoteAddr,
		ConnType:   connType,
		Host:       host,
		Crypt:      crypt,
		Compress:   compress,
		LocalProxy: localProxy,
		Option:     options,
	}
}

func newOptions(opts ...Option) Options {
	opt := Options{
		Timeout: defaultTimeOut,
		NeedAck: false,
	}
	for _, o := range opts {
		o(&opt)
	}
	return opt
}

func LinkTimeout(t time.Duration) Option {
	return func(opt *Options) {
		opt.Timeout = t
	}
}

func WithAck(enabled bool) Option {
	return func(opt *Options) {
		opt.NeedAck = enabled
	}
}
