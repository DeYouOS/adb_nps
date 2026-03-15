// Package mcp 提供基于 MCP (Model Context Protocol) 协议的 AI 远程调试接口，
// 允许 AI 工具通过标准 MCP 协议访问 NPS 管理的 ADB 设备。
package mcp

import (
	"context"
	"fmt"
	"net/http"

	"github.com/beego/beego"
	"github.com/djylb/nps/lib/logs"
	"github.com/mark3labs/mcp-go/server"
)

// mcpAuthContextKey 用于在 HTTP 请求 context 中传递认证头的私有键类型，
// 避免与其他包的 context key 发生冲突。
type mcpAuthContextKey struct{}

// Start 启动 MCP 服务器。
// 从 beego 配置读取端口、鉴权密钥、ADB 地址等参数，
// 注册所有工具后以 StreamableHTTP 方式监听。
// 若 mcp_port 为 "0" 或空，则跳过启动（禁用）。
func Start() {
	// 读取 MCP 服务端口，默认 8028
	port := beego.AppConfig.DefaultString("mcp_port", "8028")
	if port == "" || port == "0" {
		logs.Info("MCP 服务已禁用（mcp_port=0 或为空）")
		return
	}

	// 读取可选鉴权密钥
	authKey := beego.AppConfig.DefaultString("mcp_auth_key", "")

	adbAddr = beego.AppConfig.DefaultString("mcp_adb_addr", "127.0.0.1:5037")
	adbConnectAddr = beego.AppConfig.DefaultString("mcp_adb_connect_addr", "")
	adbPairAddr = beego.AppConfig.DefaultString("mcp_adb_pair_addr", "")

	// 构造服务器选项列表
	var opts []server.ServerOption

	// 若配置了鉴权密钥，通过请求初始化钩子校验 Authorization 头
	if authKey != "" {
		hooks := &server.Hooks{}
		// AddOnRequestInitialization 在每次 JSON-RPC 请求初始化时触发，用于鉴权
		hooks.AddOnRequestInitialization(func(
			ctx context.Context,
			id any,
			message any,
		) error {
			// 从 context 中取出由 WithHTTPContextFunc 注入的 Authorization 头值
			val, _ := ctx.Value(mcpAuthContextKey{}).(string)
			expected := "Bearer " + authKey
			if val != expected {
				return fmt.Errorf("MCP 鉴权失败：Authorization 头不正确")
			}
			return nil
		})
		opts = append(opts, server.WithHooks(hooks))
		logs.Info("MCP 鉴权已启用")
	}

	// 创建 MCP 服务实例，名称 nps-mcp，版本 0.1.0
	mcpServer := server.NewMCPServer("nps-mcp", "0.1.0", opts...)

	// 注册 NPS 内部工具（list_devices、ping_device）
	registerNPSTools(mcpServer)

	// 注册 ADB 工具（adb_devices、adb_shell 等）
	registerADBTools(mcpServer)

	// 通过 WithHTTPContextFunc 将 Authorization 头注入 context，供鉴权钩子使用
	httpOpts := []server.StreamableHTTPOption{
		server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			return context.WithValue(ctx, mcpAuthContextKey{}, r.Header.Get("Authorization"))
		}),
	}

	// 创建 Streamable HTTP 服务器（MCP over HTTP 标准传输方式）
	httpServer := server.NewStreamableHTTPServer(mcpServer, httpOpts...)

	addr := ":" + port
	logs.Info("MCP 服务启动，监听地址 %s（ADB 地址: %s）", addr, adbAddr)

	// 阻塞监听（由调用方以 goroutine 方式启动，退出时记录错误）
	if err := httpServer.Start(addr); err != nil {
		logs.Error("MCP 服务启动失败: %v", err)
	}
}
