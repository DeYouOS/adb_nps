package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	defaultADBPath         = "adb"
	defaultOutputLimit     = 64 * 1024
	defaultBinaryLimit     = 10 * 1024 * 1024
	defaultShellTimeout    = 30 * time.Second
	defaultInstallTimeout  = 2 * time.Minute
	defaultScreencapTimout = 30 * time.Second
)

// authContextKey 用于在 HTTP 模式下传递 Authorization 头的上下文键
type authContextKey struct{}

// ============================================================
// NPS Web API 客户端
// ============================================================

// npsClient 封装了与 NPS Web 管理接口的通信逻辑，
// 通过 MD5(authKey + timestamp) 进行认证
type npsClient struct {
	baseURL    string       // NPS Web 管理地址，如 http://101.34.243.224:8081
	authKey    string       // conf/nps.conf 里的 auth_key 原始值
	httpClient *http.Client // 带 10 秒超时
}

// buildAuthQuery 构造 NPS Web API 认证所需的 query 参数。
// 认证机制：timestamp = 当前 Unix 秒级时间戳，
// auth_key = MD5(配置的authKey + timestamp字符串)，
// 有效期：timestamp 和服务器时间差不超过 20 秒
func (c *npsClient) buildAuthQuery() string {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	hash := md5.Sum([]byte(c.authKey + timestamp))
	md5Hex := hex.EncodeToString(hash[:])
	return "auth_key=" + md5Hex + "&timestamp=" + timestamp
}

// doPost 向 NPS Web API 发送 POST 请求，返回响应 body 字节。
// NPS Web 控制器中 /client/list 等接口，GET 返回 HTML 页面，POST 才返回 JSON，
// 因此统一使用 POST 方法。
// path 为 API 路径（如 /client/list），extraParams 为额外的 query 参数（拼接到 URL）。
// 非 200 状态码会返回错误
func (c *npsClient) doPost(path string, extraParams string) ([]byte, error) {
	reqURL := c.baseURL + path + "?" + c.buildAuthQuery()
	if extraParams != "" {
		reqURL += "&" + extraParams
	}
	resp, err := c.httpClient.Post(reqURL, "application/x-www-form-urlencoded", nil)
	if err != nil {
		return nil, fmt.Errorf("NPS 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 NPS 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("NPS 返回非 200 状态码: %d, body: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// ============================================================
// NPS API 响应结构体
// ============================================================

// npsClientListResp 是 NPS /client/list 接口的响应结构
type npsClientListResp struct {
	Rows  []npsClientInfo `json:"rows"`
	Total int             `json:"total"`
}

// npsClientInfo 表示 NPS 客户端设备信息
type npsClientInfo struct {
	Id             int    `json:"Id"`
	Remark         string `json:"Remark"`
	Addr           string `json:"Addr"`
	IsConnect      bool   `json:"IsConnect"`
	Version        string `json:"Version"`
	Status         bool   `json:"Status"`
	LastOnlineTime string `json:"LastOnlineTime"`
}

// npsPingResp 是 NPS /client/ping_client 接口的响应结构
type npsPingResp struct {
	Code int `json:"code"`
	RTT  int `json:"rtt"`
}

// npsTunnelListResp 是 NPS /index/get_tunnel 接口的响应结构
type npsTunnelListResp struct {
	Rows  []npsTunnelInfo `json:"rows"`
	Total int             `json:"total"`
}

// npsTunnelInfo 表示 NPS 隧道信息
type npsTunnelInfo struct {
	Id     int    `json:"Id"`
	Port   int    `json:"Port"`
	Mode   string `json:"Mode"`
	Target struct {
		TargetStr string `json:"TargetStr"`
	} `json:"Target"`
	Remark string `json:"Remark"`
	Status bool   `json:"Status"`
}

// ============================================================
// 应用主结构
// ============================================================

// app 是应用的核心结构，持有 ADB 和 NPS 相关配置
type app struct {
	adbPath      string
	connectAddr  string // 保留，可选静态覆盖
	pairAddr     string // 保留，可选静态覆盖
	outputLimit  int
	binaryLimit  int
	shellTimeout time.Duration
	nps          *npsClient // nil 表示未配置 NPS
}

func main() {
	transportMode := flag.String("transport", "stdio", "transport: stdio or http")
	listenAddr := flag.String("addr", "127.0.0.1:18028", "http listen address")
	endpointPath := flag.String("endpoint", "/mcp", "http endpoint path")
	authKey := flag.String("auth-key", "", "optional bearer auth key for http mode")
	adbPath := flag.String("adb-path", defaultADBPath, "local adb executable path")
	connectAddr := flag.String("connect-addr", "", "address returned by adb connect tool")
	pairAddr := flag.String("pair-addr", "", "address returned by adb pair tool")
	npsURL := flag.String("nps-url", "", "NPS Web 管理地址（如 http://101.34.243.224:8080）")
	npsAuthKey := flag.String("nps-auth-key", "", "NPS 的 auth_key（与 conf/nps.conf 中配置的一致）")
	npsProxy := flag.String("nps-proxy", "", "NPS 请求代理（如 http://127.0.0.1:7897）")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 初始化 NPS 客户端（仅在配置了 --nps-url 时创建）
	var nps *npsClient
	if *npsURL != "" {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if *npsProxy != "" {
			proxyURL, err := url.Parse(*npsProxy)
			if err != nil {
				log.Fatalf("无效的 --nps-proxy 地址: %v", err)
			}
			transport.Proxy = http.ProxyURL(proxyURL)
		}
		nps = &npsClient{
			baseURL: strings.TrimRight(*npsURL, "/"),
			authKey: *npsAuthKey,
			httpClient: &http.Client{
				Timeout:   10 * time.Second,
				Transport: transport,
			},
		}
	}

	a := &app{
		adbPath:      *adbPath,
		connectAddr:  *connectAddr,
		pairAddr:     *pairAddr,
		outputLimit:  defaultOutputLimit,
		binaryLimit:  defaultBinaryLimit,
		shellTimeout: defaultShellTimeout,
		nps:          nps,
	}

	mcpServer := server.NewMCPServer("local-adb-mcp", "0.1.0")
	a.registerTools(mcpServer)

	switch strings.ToLower(strings.TrimSpace(*transportMode)) {
	case "stdio":
		stdioServer := server.NewStdioServer(mcpServer)
		if err := stdioServer.Listen(ctx, os.Stdin, os.Stdout); err != nil {
			log.Fatal(err)
		}
	case "http":
		a.startHTTP(ctx, mcpServer, *listenAddr, *endpointPath, *authKey)
	default:
		log.Fatalf("unsupported transport: %s", *transportMode)
	}
}

// startHTTP 启动 HTTP 模式的 MCP 服务，支持可选的 Bearer 鉴权
func (a *app) startHTTP(ctx context.Context, mcpServer *server.MCPServer, listenAddr, endpointPath, authKey string) {
	var opts []server.ServerOption
	if authKey != "" {
		hooks := &server.Hooks{}
		hooks.AddOnRequestInitialization(func(ctx context.Context, id any, message any) error {
			actual, _ := ctx.Value(authContextKey{}).(string)
			expected := "Bearer " + authKey
			if actual != expected {
				return fmt.Errorf("MCP 鉴权失败：Authorization 头不正确")
			}
			return nil
		})
		opts = append(opts, server.WithHooks(hooks))
	}

	wrapped := server.NewMCPServer("local-adb-mcp", "0.1.0", opts...)
	a.registerTools(wrapped)

	httpServer := server.NewStreamableHTTPServer(
		wrapped,
		server.WithEndpointPath(endpointPath),
		server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			return context.WithValue(ctx, authContextKey{}, r.Header.Get("Authorization"))
		}),
	)

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.Start(listenAddr)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}
}

// registerTools 注册所有 MCP 工具到服务器，包括 11 个 ADB 工具和可选的 NPS 工具
func (a *app) registerTools(s *server.MCPServer) {
	// ---- 11 个 ADB 工具（保持不变） ----
	s.AddTool(mcp.NewTool("ADB设备列表", mcp.WithDescription("列出当前机器上 adb 已连接的所有设备（等同于 adb devices -l）")), a.handleDevices)
	s.AddTool(
		mcp.NewTool("执行命令",
			mcp.WithDescription("在指定 ADB 设备上执行本地 adb shell 命令，返回命令输出（最多 64 KB）"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号（adb devices 输出的第一列）")),
			mcp.WithString("command", mcp.Required(), mcp.Description("要执行的 shell 命令，例如：ls /data/local/tmp")),
		),
		a.handleShell,
	)
	s.AddTool(
		mcp.NewTool("截屏",
			mcp.WithDescription("截取指定 ADB 设备的屏幕截图，以 base64 编码的 PNG 图片形式返回"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
		),
		a.handleScreencap,
	)
	s.AddTool(
		mcp.NewTool("查看日志",
			mcp.WithDescription("从指定 ADB 设备获取 logcat 日志，支持行数限制和过滤器"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithNumber("lines", mcp.DefaultNumber(50), mcp.Description("返回的日志行数，默认 50")),
			mcp.WithString("filter", mcp.Description("logcat 过滤器表达式，例如：ActivityManager:I *:S（留空不过滤）")),
		),
		a.handleLogcat,
	)
	s.AddTool(
		mcp.NewTool("应用列表",
			mcp.WithDescription("列出指定 ADB 设备上已安装的应用包名列表"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithString("filter", mcp.Description("包名关键词过滤，留空返回所有包")),
		),
		a.handlePackages,
	)
	s.AddTool(
		mcp.NewTool("安装应用",
			mcp.WithDescription("在指定 ADB 设备上安装本地 APK 文件（等同于 adb -s <serial> install -r <path>）"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithString("path", mcp.Required(), mcp.Description("当前机器上的 APK 本地路径，例如：/tmp/app.apk")),
		),
		a.handleInstall,
	)
	s.AddTool(mcp.NewTool("ADB配对", mcp.WithDescription("返回 adb pair 命令，Android 11+ 无线调试首次连接前需要先配对")), a.handlePair)
	s.AddTool(mcp.NewTool("开启ADB调试", mcp.WithDescription("返回 adb pair + adb connect 命令，AI 在本地终端执行即可直连远程设备（用于 push/pull 等本地操作）")), a.handleConnect)
	s.AddTool(mcp.NewTool("关闭ADB调试", mcp.WithDescription("返回 adb disconnect 命令，AI 在本地终端执行断开远程设备连接")), a.handleDisconnect)
	s.AddTool(
		mcp.NewTool("系统属性",
			mcp.WithDescription("读取指定 ADB 设备的 Android 系统属性，可指定单个属性名或不传返回全部"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithString("prop", mcp.Description("属性名称，例如：ro.build.version.release（留空返回所有属性）")),
		),
		a.handleGetprop,
	)
	s.AddTool(
		mcp.NewTool("重启设备",
			mcp.WithDescription("重启指定 ADB 设备，可选进入 recovery 或 bootloader 模式"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithString("mode", mcp.Description("重启模式：留空为正常重启，recovery 为进入 recovery，bootloader 为进入 bootloader")),
		),
		a.handleReboot,
	)

	// ---- NPS 工具（仅在配置了 NPS 地址时注册） ----
	if a.nps != nil {
		s.AddTool(
			mcp.NewTool("NPS设备列表",
				mcp.WithDescription("查询 NPS 服务器上所有已注册的客户端设备及其在线状态"),
			),
			a.handleNPSClientList,
		)
		s.AddTool(
			mcp.NewTool("NPS连通测试",
				mcp.WithDescription("测试指定 NPS 客户端的网络连通性，返回往返延迟（RTT）"),
				mcp.WithNumber("client_id", mcp.Required(), mcp.Description("NPS 客户端 ID（整数）")),
			),
			a.handleNPSPing,
		)
		s.AddTool(
			mcp.NewTool("隧道列表",
				mcp.WithDescription("查询 NPS 服务器上的隧道列表，可按客户端 ID 或类型筛选"),
				mcp.WithNumber("client_id", mcp.Description("按客户端 ID 筛选（0 表示全部）")),
				mcp.WithString("type", mcp.Description("隧道类型筛选：tcp/udp/http/socks5/secret/p2p/file（留空返回全部）")),
			),
			a.handleNPSTunnelList,
		)
	}
}

// ============================================================
// ADB 工具辅助方法
// ============================================================

// runADBText 执行 adb 命令并返回文本输出，超过 outputLimit 时截断
func (a *app) runADBText(ctx context.Context, args ...string) (string, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, a.adbPath, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("执行 adb 命令失败（%s %s）: %s", a.adbPath, strings.Join(args, " "), message)
	}
	output := stdout.String()
	if strings.TrimSpace(output) == "" {
		output = stderr.String()
	}
	if len(output) > a.outputLimit {
		output = output[:a.outputLimit] + "\n[输出已截断，超过 64 KB 限制]"
	}
	return output, nil
}

// runADBBinary 执行 adb 命令并返回二进制输出，超过 binaryLimit 时报错
func (a *app) runADBBinary(ctx context.Context, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, a.adbPath, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("执行 adb 命令失败（%s %s）: %s", a.adbPath, strings.Join(args, " "), message)
	}
	data := stdout.Bytes()
	if len(data) == 0 {
		return nil, fmt.Errorf("adb 命令没有返回任何二进制数据")
	}
	if len(data) > a.binaryLimit {
		return nil, fmt.Errorf("adb 二进制输出超过 %d 字节限制", a.binaryLimit)
	}
	return data, nil
}

// serialArgs 返回 adb -s <serial> 的参数切片
func serialArgs(serial string) []string {
	return []string{"-s", serial}
}

// ============================================================
// 11 个 ADB 工具 Handler（逻辑完全不变）
// ============================================================

// handleDevices 列出所有已连接的 ADB 设备
func (a *app) handleDevices(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()
	output, err := a.runADBText(callCtx, "devices", "-l")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(output), nil
}

// handleShell 在指定设备上执行 shell 命令
func (a *app) handleShell(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	command, err := req.RequireString("command")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	shellArgs := strings.Fields(command)
	if len(shellArgs) == 0 {
		return mcp.NewToolResultError("command 不能为空"), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()
	args := append(serialArgs(serial), "shell")
	args = append(args, shellArgs...)
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(output), nil
}

// handleScreencap 截取设备屏幕截图，返回 base64 编码的 PNG
func (a *app) handleScreencap(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, defaultScreencapTimout)
	defer cancel()
	args := append(serialArgs(serial), "exec-out", "screencap", "-p")
	pngData, err := a.runADBBinary(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultImage("设备屏幕截图", base64.StdEncoding.EncodeToString(pngData), "image/png"), nil
}

// handleLogcat 获取设备 logcat 日志
func (a *app) handleLogcat(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	lines := req.GetInt("lines", 50)
	if lines <= 0 {
		lines = 50
	}
	filter := req.GetString("filter", "")
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()
	args := append(serialArgs(serial), "logcat", "-d", "-t", fmt.Sprintf("%d", lines))
	if filter != "" {
		args = append(args, strings.Fields(filter)...)
	}
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(output), nil
}

// handlePackages 列出设备上已安装的应用包名
func (a *app) handlePackages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	filter := req.GetString("filter", "")
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()
	args := append(serialArgs(serial), "shell", "pm", "list", "packages")
	if filter != "" {
		args = append(args, filter)
	}
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(output), nil
}

// handleInstall 在设备上安装 APK 文件
func (a *app) handleInstall(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, defaultInstallTimeout)
	defer cancel()
	args := append(serialArgs(serial), "install", "-r", path)
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(output), nil
}

// handlePair 返回 adb pair 配对命令
func (a *app) handlePair(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if a.pairAddr == "" {
		return mcp.NewToolResultError("未配置 pair-addr，无法生成配对命令"), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("在本地终端执行以下命令配对设备（设备端会弹出配对码，输入即可）：\n\n%s pair %s", a.adbPath, a.pairAddr)), nil
}

// handleConnect 返回 adb connect 连接命令
func (a *app) handleConnect(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if a.connectAddr == "" {
		return mcp.NewToolResultError("未配置 connect-addr，无法生成连接命令"), nil
	}
	var builder strings.Builder
	builder.WriteString("在本地终端按顺序执行以下命令直连远程设备：\n\n")
	if a.pairAddr != "" {
		builder.WriteString(fmt.Sprintf("# 第一步：配对（首次连接需要，设备端会弹出配对码）\n%s pair %s\n\n", a.adbPath, a.pairAddr))
	}
	builder.WriteString(fmt.Sprintf("# 第二步：连接\n%s connect %s\n\n", a.adbPath, a.connectAddr))
	builder.WriteString("连接后可直接使用本地 adb 进行 push/pull/install 等操作。")
	return mcp.NewToolResultText(builder.String()), nil
}

// handleDisconnect 返回 adb disconnect 断开命令
func (a *app) handleDisconnect(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if a.connectAddr == "" {
		return mcp.NewToolResultError("未配置 connect-addr，无法生成断开命令"), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("在本地终端执行以下命令断开远程设备：\n\n%s disconnect %s", a.adbPath, a.connectAddr)), nil
}

// handleGetprop 读取设备的 Android 系统属性
func (a *app) handleGetprop(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	prop := req.GetString("prop", "")
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()
	args := append(serialArgs(serial), "shell", "getprop")
	if prop != "" {
		args = append(args, prop)
	}
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(output), nil
}

// handleReboot 重启设备，支持 normal/recovery/bootloader 模式
func (a *app) handleReboot(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	mode := req.GetString("mode", "")
	var rebootArgs []string
	switch mode {
	case "", "normal":
		rebootArgs = []string{"reboot"}
	case "recovery":
		rebootArgs = []string{"reboot", "recovery"}
	case "bootloader":
		rebootArgs = []string{"reboot", "bootloader"}
	default:
		return mcp.NewToolResultError(fmt.Sprintf("不支持的重启模式：%q，可选值：空（正常重启）、recovery、bootloader", mode)), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := append(serialArgs(serial), rebootArgs...)
	_, _ = a.runADBText(callCtx, args...)
	label := "正常重启"
	if mode == "recovery" {
		label = "recovery 模式"
	}
	if mode == "bootloader" {
		label = "bootloader 模式"
	}
	return mcp.NewToolResultText(fmt.Sprintf("已向设备 %s 发送重启命令（模式：%s）", serial, label)), nil
}

// ============================================================
// 3 个 NPS 工具 Handler
// ============================================================

// handleNPSClientList 查询 NPS 服务器上所有已注册的客户端设备及其在线状态
func (a *app) handleNPSClientList(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	body, err := a.nps.doPost("/client/list", "offset=0&limit=9999&search=&sort=&order=")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("查询 NPS 设备列表失败: %v", err)), nil
	}
	var resp npsClientListResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("解析 NPS 响应失败: %v", err)), nil
	}
	if len(resp.Rows) == 0 {
		return mcp.NewToolResultText("当前没有已注册的客户端设备"), nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("共 %d 个客户端设备：\n\n", resp.Total))
	for _, c := range resp.Rows {
		status := "离线 ✗"
		if c.IsConnect {
			status = "在线 ✓"
		}
		sb.WriteString(fmt.Sprintf("ID: %d | 备注: %s | 地址: %s | 状态: %s | 版本: %s\n", c.Id, c.Remark, c.Addr, status, c.Version))
	}
	return mcp.NewToolResultText(sb.String()), nil
}

// handleNPSPing 测试指定 NPS 客户端的网络连通性，返回 RTT
func (a *app) handleNPSPing(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	clientID, err := req.RequireInt("client_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	body, err := a.nps.doPost("/client/pingclient", fmt.Sprintf("id=%d", clientID))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("NPS 连通测试失败: %v", err)), nil
	}
	var resp npsPingResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("解析 NPS 响应失败: %v", err)), nil
	}
	if resp.Code == 1 {
		return mcp.NewToolResultText(fmt.Sprintf("客户端 %d 的 RTT：%d ms", clientID, resp.RTT)), nil
	}
	return mcp.NewToolResultError(fmt.Sprintf("客户端 %d 离线或不存在", clientID)), nil
}

// handleNPSTunnelList 查询 NPS 服务器上的隧道列表，支持按客户端 ID 和类型筛选
func (a *app) handleNPSTunnelList(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// 获取可选参数 client_id，默认为 0（全部）
	clientID := req.GetInt("client_id", 0)
	// 获取可选参数 type，默认为空（全部）
	tunnelType := req.GetString("type", "")
	params := fmt.Sprintf("offset=0&limit=9999&client_id=%d&type=%s&search=&sort=&order=", clientID, tunnelType)
	body, err := a.nps.doPost("/index/gettunnel", params)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("查询隧道列表失败: %v", err)), nil
	}
	var resp npsTunnelListResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("解析 NPS 响应失败: %v", err)), nil
	}
	if len(resp.Rows) == 0 {
		return mcp.NewToolResultText("当前没有隧道记录"), nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("共 %d 条隧道记录：\n\n", resp.Total))
	for _, t := range resp.Rows {
		status := "已停止"
		if t.Status {
			status = "运行中"
		}
		sb.WriteString(fmt.Sprintf("ID: %d | 类型: %s | 端口: %d | 目标: %s | 备注: %s | 状态: %s\n", t.Id, t.Mode, t.Port, t.Target.TargetStr, t.Remark, status))
	}
	return mcp.NewToolResultText(sb.String()), nil
}
