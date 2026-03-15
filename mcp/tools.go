// tools.go 包含 NPS 内部工具，通过读取 Bridge 和 JsonDb 数据提供
// 设备列表查询和 Ping 延迟检测功能，供 MCP 客户端（AI）调用。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/djylb/nps/bridge"
	"github.com/djylb/nps/lib/file"
	npsserver "github.com/djylb/nps/server"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// DeviceInfo 描述一个 NPS 客户端（设备）的基本信息，
// 用于 list_devices 工具的 JSON 返回结构。
type DeviceInfo struct {
	// NPS 客户端 ID
	ID int `json:"id"`
	// 备注名称
	Remark string `json:"remark"`
	// 客户端 IP 地址（最近一次连接）
	Addr string `json:"addr"`
	// 是否当前在线
	IsConnect bool `json:"is_connect"`
	// 客户端软件版本
	Version string `json:"version"`
	// 是否启用（管理员配置）
	Status bool `json:"status"`
	// 当前活跃隧道节点数
	NodeCount int `json:"node_count"`
}

// registerNPSTools 向 MCP 服务器注册 NPS 内部工具：
//   - list_devices：列出所有已配置的 NPS 客户端及其在线状态
//   - ping_device：对指定客户端进行 Ping 延迟测量
func registerNPSTools(s *server.MCPServer) {
	// list_devices：枚举所有设备（包括离线设备）
	s.AddTool(
		mcp.NewTool("list_devices",
			mcp.WithDescription("列出所有已配置的 NPS 客户端设备及其在线状态、版本和节点数"),
		),
		handleListDevices,
	)

	// ping_device：测量到指定设备的往返延迟（仅在线设备有效）
	s.AddTool(
		mcp.NewTool("ping_device",
			mcp.WithDescription("Ping 指定 NPS 客户端，测量往返延迟（RTT），单位毫秒。设备离线时返回错误。"),
			mcp.WithNumber("client_id",
				mcp.Required(),
				mcp.Description("目标 NPS 客户端 ID（整数）"),
			),
		),
		handlePingDevice,
	)
}

// handleListDevices 处理 list_devices 工具调用。
// 遍历 JsonDb.Clients（所有已配置客户端）并结合 Bridge.Client（在线状态与节点数），
// 返回 JSON 格式的设备列表。
func handleListDevices(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var devices []DeviceInfo

	// 遍历 JsonDb 中所有已持久化的客户端记录
	file.GetDb().JsonDb.Clients.Range(func(key, value any) bool {
		c, ok := value.(*file.Client)
		if !ok {
			return true
		}

		info := DeviceInfo{
			ID:        c.Id,
			Remark:    c.Remark,
			Addr:      c.Addr,
			IsConnect: c.IsConnect,
			Version:   c.Version,
			Status:    c.Status,
		}

		// 从 Bridge.Client 中读取活跃连接数（NodeCount）
		if npsserver.Bridge != nil {
			if raw, ok := npsserver.Bridge.Client.Load(c.Id); ok {
				if bc, ok := raw.(*bridge.Client); ok {
					info.NodeCount = bc.NodeCount()
				}
			}
		}

		devices = append(devices, info)
		return true
	})

	// 序列化为 JSON 字符串返回
	data, err := json.MarshalIndent(devices, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("序列化设备列表失败: %v", err)), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}

// handlePingDevice 处理 ping_device 工具调用。
// 解析 client_id 参数，调用 server.PingClient 测量 RTT。
// 返回值为毫秒数（正整数），-1 表示设备离线或连接失败。
func handlePingDevice(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	clientID, err := req.RequireInt("client_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	if clientID <= 0 {
		return mcp.NewToolResultError("client_id 必须为正整数"), nil
	}

	// 调用 NPS 内置 Ping 函数，返回 RTT（毫秒），-1 表示离线
	rtt := npsserver.PingClient(clientID, "")
	if rtt < 0 {
		return mcp.NewToolResultError(
			fmt.Sprintf("客户端 %d 离线或连接失败", clientID),
		), nil
	}

	return mcp.NewToolResultText(
		fmt.Sprintf("客户端 %d 的 RTT：%d ms", clientID, rtt),
	), nil
}


