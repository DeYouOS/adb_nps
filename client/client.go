package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
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
	// 识别 ADB 远程控制命令
	if lk.ConnType == "adbctl" {
		// 先回复 ACK（服务端设置了 NeedAck）
		if lk.Option.NeedAck {
			if err := conn.WriteACK(src, lk.Option.Timeout); err != nil {
				logs.Warn("发送 ADBCTL ACK 失败: %v", err)
				_ = src.Close()
				return
			}
		}
		// 从 Host 字段读取命令（start/stop/restart/reboot）
		s.handleAdbCtl(src, lk.Host)
		return
	}

	// 非 ADBCTL 类型，继续原有流程
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

// handleAdbCtl 处理来自 NPS 服务端的 ADB 远程控制命令。
// NPC 客户端本身以 root 权限运行在 Android 上，可以直接执行 stop/start adbd。
// 支持的命令：start（启动 adbd）、stop（停止 adbd）、restart（重启 adbd）、reboot（重启手机）。
// reboot 命令执行前会先断开与服务端的连接，确保 Web UI 状态立即更新为离线。
func (s *TRPClient) handleAdbCtl(src net.Conn, command string) {
	var stdout, stderr bytes.Buffer
	var cmd *exec.Cmd

	switch command {
	case "start":
		cmd = exec.Command("start", "adbd")
	case "stop":
		cmd = exec.Command("stop", "adbd")
	case "restart":
		// restart = stop + start
		cmd = exec.Command("sh", "-c", "stop adbd; sleep 1; start adbd")
	case "reboot":
		// 一键重启手机：先断开 NPS 连接让服务端更新状态，再执行 reboot
		cmd = exec.Command("reboot")
	default:
		resp := conn.AdbCtlResponse{
			Success: false,
			Message: "不支持的 ADBCTL 命令: " + command,
		}
		writeAdbCtlResponse(src, &resp)
		_ = src.Close()
		return
	}

	// reboot 命令：先回复服务端成功，再断开连接，最后执行 reboot
	if command == "reboot" {
		resp := conn.AdbCtlResponse{
			Success: true,
			Message: "正在重启手机，连接已断开",
		}
		writeAdbCtlResponse(src, &resp)
		_ = src.Close()
		// 先断开 NPS 主连接，让服务端立即感知客户端下线
		logs.Info("ADBCTL reboot: 断开 NPS 连接")
		s.Close()
		time.Sleep(500 * time.Millisecond)
		logs.Info("ADBCTL reboot: 执行 reboot")
		_ = cmd.Run()
		return
	}

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	resp := conn.AdbCtlResponse{}
	if err != nil {
		resp.Success = false
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		resp.Message = fmt.Sprintf("执行 '%s adbd' 失败: %s", command, errMsg)
		logs.Warn("ADBCTL %s 失败: %v", command, err)
	} else {
		resp.Success = true
		output := strings.TrimSpace(stdout.String())
		if output != "" {
			resp.Message = fmt.Sprintf("执行 '%s adbd' 成功: %s", command, output)
		} else {
			resp.Message = fmt.Sprintf("执行 '%s adbd' 成功", command)
		}
		logs.Info("ADBCTL %s 成功", command)
	}

	writeAdbCtlResponse(src, &resp)
	_ = src.Close()
}

// writeAdbCtlResponse 将 ADBCTL 响应按 [4字节长度][JSON] 格式写入连接。
// 协议格式与 WriteAdbCtlMessage 保持一致。
func writeAdbCtlResponse(w io.Writer, resp *conn.AdbCtlResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("序列化 ADBCTL 响应失败: %w", err)
	}
	// 先写 4 字节长度头（小端序），与 WriteAdbCtlMessage 的格式兼容
	if err := binary.Write(w, binary.LittleEndian, int32(len(data))); err != nil {
		return fmt.Errorf("写入 ADBCTL 响应长度失败: %w", err)
	}
	_, err = w.Write(data)
	return err
}
