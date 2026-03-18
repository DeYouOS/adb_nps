package client

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/djylb/nps/lib/common"
	"github.com/djylb/nps/lib/conn"
	"github.com/djylb/nps/lib/logs"
	"github.com/djylb/nps/lib/mux"
)

// TRPClient 是精简后的反向代理客户端，只保留 TCP/TLS 到服务端的主链路。
// 最终效果：完成鉴权、建立 mux 隧道、接收服务端下发的转发请求并回连目标地址。
type TRPClient struct {
	svrAddr        string
	bridgeConnType string
	vKey           string
	uuid           string
	tunnel         *mux.Mux
	signal         *conn.Conn
	ticker         *time.Ticker
	disconnectTime int
	ctx            context.Context
	cancel         context.CancelFunc
	once           sync.Once
}

var NowStatus int

// NewRPClient 创建最小化客户端实例。
// 参数分别为服务端地址、验证密钥、连接类型以及断连超时时间。
func NewRPClient(svrAddr, vKey, bridgeConnType string, disconnectTime int) *TRPClient {
	return &TRPClient{
		svrAddr:        svrAddr,
		vKey:           vKey,
		bridgeConnType: bridgeConnType,
		disconnectTime: disconnectTime,
	}
}

// Start 启动客户端主循环。
// 它会先建立控制连接，再建立 mux 通道，最后持续处理服务端分发的转发请求。
func (s *TRPClient) Start(ctx context.Context) {
	s.ctx, s.cancel = context.WithCancel(ctx)
	defer s.Close()
	NowStatus = 0

	if Ver < 5 {
		c, uuid, err := NewConn(s.bridgeConnType, s.vKey, s.svrAddr)
		if err != nil {
			logs.Error("连接服务端失败，五秒后将重连: %v", err)
			return
		}
		if s.uuid == "" {
			s.uuid = uuid
		}
		if err := SendType(c, common.WORK_MAIN, s.uuid); err != nil {
			logs.Error("发送主连接类型失败，五秒后将重连: %v", err)
			_ = c.Close()
			return
		}
		logs.Info("已连接服务端 %s", s.svrAddr)
		s.signal = c
	}

	if !s.newChan() {
		return
	}
	if Ver > 4 {
		mc, err := s.tunnel.NewConn()
		if err != nil {
			logs.Error("获取主控制流失败，可能是协议版本不匹配: %v", err)
			return
		}
		mc.SetPriority()
		c := conn.NewConn(mc)
		if err := SendType(c, common.WORK_MAIN, s.uuid); err != nil {
			logs.Error("发送主连接类型失败，五秒后将重连: %v", err)
			_ = mc.Close()
			return
		}
		logs.Info("已连接服务端 %s", s.svrAddr)
		s.signal = c
	}

	go s.ping()
	NowStatus = 1
	s.handleMain()
}

// handleMain 仅保留主控制连接的存活检测。
// 当前精简版不再处理 UDP/P2P 指令，只要主控制流断开就整体重连。
func (s *TRPClient) handleMain() {
	defer s.Close()
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		if _, err := s.signal.ReadFlag(); err != nil {
			logs.Error("读取服务端控制消息失败，当前连接结束: %v", err)
			return
		}
	}
}

// newChan 建立 mux 通道，并持续接收服务端下发的新连接。
func (s *TRPClient) newChan() bool {
	tunnel, uuid, err := NewConn(s.bridgeConnType, s.vKey, s.svrAddr)
	if err != nil {
		logs.Error("连接服务端 %s 失败: %v", s.svrAddr, err)
		logs.Warn("连接失败，五秒后将重连")
		return false
	}
	if s.uuid == "" {
		s.uuid = uuid
	}
	if err := SendType(tunnel, common.WORK_CHAN, s.uuid); err != nil {
		logs.Error("发送通道类型到服务端 %s 失败: %v", s.svrAddr, err)
		logs.Warn("连接失败，五秒后将重连")
		_ = tunnel.Close()
		return false
	}
	s.tunnel = mux.NewMux(tunnel.Conn, s.bridgeConnType, s.disconnectTime, true)

	go func() {
		defer func() { _ = tunnel.Close() }()
		for {
			select {
			case <-s.ctx.Done():
				return
			default:
			}
			src, err := s.tunnel.Accept()
			if err != nil {
				logs.Warn("mux 接收新流失败: %v", err)
				s.Close()
				return
			}
			go s.handleChan(src)
		}
	}()
	return true
}

// handleChan 处理服务端派发的单条转发连接。
// 它会读取链路信息、回 ACK，然后直接连接目标地址并双向拷贝数据。
func (s *TRPClient) handleChan(src net.Conn) {
	lk, err := conn.NewConn(src).GetLinkInfo()
	if err != nil || lk == nil {
		_ = src.Close()
		logs.Error("读取服务端链路信息失败: %v", err)
		return
	}
	if lk.Option.NeedAck {
		if err := conn.WriteACK(src, lk.Option.Timeout); err != nil {
			logs.Warn("发送 ACK 失败: %v", err)
			_ = src.Close()
			return
		}
	}
	if lk.Host == "" {
		logs.Trace("收到空目标地址，关闭连接，远端=%s", lk.RemoteAddr)
		_ = src.Close()
		return
	}

	lk.Host = common.FormatAddress(lk.Host)
	targetConn, err := net.DialTimeout(lk.ConnType, lk.Host, lk.Option.Timeout)
	if err != nil {
		logs.Warn("连接目标 %s 失败: %v", lk.Host, err)
		_ = src.Close()
		return
	}
	logs.Trace("建立 %s 转发，目标=%s，远端=%s", lk.ConnType, lk.Host, lk.RemoteAddr)
	conn.CopyWaitGroup(src, targetConn, lk.Crypt, lk.Compress, nil, nil, false, 0, nil, nil, false, lk.ConnType == "udp" && Ver > 6)
}

// ping 定时检查 mux 是否断开，断开后触发整体重连。
func (s *TRPClient) ping() {
	s.ticker = time.NewTicker(5 * time.Second)
	for {
		select {
		case <-s.ticker.C:
			if s.tunnel == nil || s.tunnel.IsClosed() {
				s.Close()
				return
			}
		case <-s.ctx.Done():
			return
		}
	}
}

// Close 幂等关闭客户端所有资源。
func (s *TRPClient) Close() {
	s.once.Do(s.closing)
}

// closing 负责释放上下文、控制连接和 mux 隧道。
func (s *TRPClient) closing() {
	NowStatus = 0
	if s.cancel != nil {
		s.cancel()
	}
	if s.tunnel != nil {
		_ = s.tunnel.Close()
		s.tunnel = nil
	}
	if s.signal != nil {
		_ = s.signal.Close()
	}
	if s.ticker != nil {
		s.ticker.Stop()
	}
}
