package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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

// npsAdbCtlResp 是 NPS /client/adbctl 接口的响应结构
type npsAdbCtlResp struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
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
	Remark       string `json:"Remark"`
	Status       bool   `json:"Status"`
	ClientID     int    `json:"-"`
	ClientRemark string `json:"-"`
	ClientOnline bool   `json:"-"`
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
		mcp.NewTool("UI布局",
			mcp.WithDescription("获取设备当前界面的 UI 元素树（uiautomator dump），返回所有可见控件的文字、坐标、类型、可点击性等信息。\n"+
				"典型用法：先调用本工具获取布局 → 根据元素坐标调用「输入操作」精确点击/输入\n"+
				"返回 JSON 数组，每个元素包含 text、bounds、class、clickable、resource-id 等属性"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
		),
		a.handleUILayout,
	)
	s.AddTool(
		mcp.NewTool("查看日志",
			mcp.WithDescription("从指定 ADB 设备获取 logcat 日志。支持三种模式：\n"+
				"1. 按行数：返回最近 N 行（默认）\n"+
				"2. 按时间：设置 since 参数返回指定时间之后的日志（格式：秒数如 '30' 表示最近30秒，或绝对时间 '2024-01-01 12:00:00.000'）\n"+
				"3. 配合「清空日志」使用：先清空缓冲区 → 执行操作 → 再调用本工具获取新产生的日志\n"+
				"注意：所有模式均为非阻塞，立即返回结果"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithNumber("lines", mcp.DefaultNumber(50), mcp.Description("返回的日志行数，默认 50（设置 since 时忽略此参数）")),
			mcp.WithString("since", mcp.Description("时间过滤：纯数字表示最近 N 秒（如 '30'），或绝对时间戳（如 '2024-01-01 12:00:00.000'）。设置后忽略 lines 参数")),
			mcp.WithString("filter", mcp.Description("logcat 过滤器表达式，例如：ActivityManager:I *:S（留空不过滤）")),
		),
		a.handleLogcat,
	)
	s.AddTool(
		mcp.NewTool("清空日志",
			mcp.WithDescription("清空指定 ADB 设备的 logcat 缓冲区。典型用法：\n"+
				"1. 调用「清空日志」清空缓冲区\n"+
				"2. 执行目标操作（安装应用、启动 Activity 等）\n"+
				"3. 调用「查看日志」获取操作期间产生的新日志\n"+
				"这样可以精确捕获特定操作的日志，避免历史日志干扰"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
		),
		a.handleLogcatClear,
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
	// 推送/拉取文件
	s.AddTool(
		mcp.NewTool("推送文件",
			mcp.WithDescription("通过 adb push 将本地文件推送到指定设备路径"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithString("local_path", mcp.Required(), mcp.Description("当前机器上的本地文件路径")),
			mcp.WithString("remote_path", mcp.Required(), mcp.Description("设备上的目标路径，例如：/sdcard/Download/file.txt")),
		),
		a.handlePush,
	)
	s.AddTool(
		mcp.NewTool("拉取文件",
			mcp.WithDescription("通过 adb pull 将设备文件拉取到当前机器"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithString("remote_path", mcp.Required(), mcp.Description("设备上的源文件路径")),
			mcp.WithString("local_path", mcp.Required(), mcp.Description("当前机器上的目标路径")),
		),
		a.handlePull,
	)
	// 应用生命周期管理
	s.AddTool(
		mcp.NewTool("卸载应用",
			mcp.WithDescription("在指定设备上卸载应用包，可选择保留数据"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithString("package", mcp.Required(), mcp.Description("应用包名")),
			mcp.WithBoolean("keep_data",
				mcp.Description("是否保留应用数据（true 时使用 -k 参数）"),
				mcp.DefaultBool(false),
			),
		),
		a.handleUninstall,
	)
	s.AddTool(
		mcp.NewTool("启动Activity",
			mcp.WithDescription("启动指定应用的 Activity，或在未指定时启动默认入口 Activity"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithString("package", mcp.Required(), mcp.Description("应用包名")),
			mcp.WithString("activity", mcp.Description("要启动的 Activity 名称，例如 .MainActivity，留空则使用 monkey 启动默认入口")),
		),
		a.handleStartActivity,
	)
	s.AddTool(
		mcp.NewTool("停止应用",
			mcp.WithDescription("通过 am force-stop 停止指定应用的所有进程"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithString("package", mcp.Required(), mcp.Description("应用包名")),
		),
		a.handleForceStop,
	)
	s.AddTool(
		mcp.NewTool("清除数据",
			mcp.WithDescription("通过 pm clear 清除指定应用的数据"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithString("package", mcp.Required(), mcp.Description("应用包名")),
		),
		a.handleClearData,
	)
	// 输入、端口与系统信息
	s.AddTool(
		mcp.NewTool("输入操作",
			mcp.WithDescription("在设备上执行 input tap/swipe/text/keyevent 等输入命令"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithString("action", mcp.Required(), mcp.Description("输入类型：tap/swipe/text/keyevent")),
			mcp.WithString("args", mcp.Required(), mcp.Description("输入参数字符串，例如：\"100 200\"、\"100 200 300 400 500\"、\"hello\"、\"KEYCODE_HOME\"")),
		),
		a.handleInput,
	)
	s.AddTool(
		mcp.NewTool("端口转发",
			mcp.WithDescription("管理 adb forward 端口转发（list/add/remove）"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithString("action", mcp.Required(), mcp.Description("操作类型：list/add/remove")),
			mcp.WithString("local", mcp.Description("本地端口描述，例如 tcp:8080")),
			mcp.WithString("remote", mcp.Description("远端端口描述，例如 tcp:8080")),
		),
		a.handleForward,
	)
	s.AddTool(
		mcp.NewTool("系统服务信息",
			mcp.WithDescription("通过 dumpsys 查看指定系统服务的信息，例如 battery、wifi、activity、meminfo、cpuinfo、package <包名>"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithString("service", mcp.Required(), mcp.Description("dumpsys 服务名称，例如 battery、wifi、activity、meminfo、cpuinfo、\"package com.example.app\"")),
		),
		a.handleDumpsys,
	)
	s.AddTool(
		mcp.NewTool("文件管理",
			mcp.WithDescription("在设备上执行基础文件操作：ls/cat/rm/mkdir/chmod/stat"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithString("action", mcp.Required(), mcp.Description("文件操作类型：ls/cat/rm/mkdir/chmod/stat")),
			mcp.WithString("path", mcp.Required(), mcp.Description("目标路径，例如：/data/local/tmp/test.txt")),
			mcp.WithString("extra", mcp.Description("额外参数，例如 chmod 的权限模式 644")),
		),
		a.handleFileOps,
	)
	s.AddTool(
		mcp.NewTool("进程列表",
			mcp.WithDescription("通过 ps 查看设备上的进程列表，可按关键字过滤"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithString("filter", mcp.Description("可选的进程名称过滤关键字")),
		),
		a.handlePs,
	)
	s.AddTool(
		mcp.NewTool("获取Root",
			mcp.WithDescription("在支持的设备上执行 adb root 或 adb unroot"),
			mcp.WithString("serial", mcp.Required(), mcp.Description("目标设备序列号")),
			mcp.WithBoolean("enable",
				mcp.Description("true 调用 adb root，false 调用 adb unroot"),
				mcp.DefaultBool(true),
			),
		),
		a.handleRoot,
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
		s.AddTool(
			mcp.NewTool("远程ADB控制",
				mcp.WithDescription("通过 NPS 隧道远程控制 NPC 设备上的 adbd 服务或重启设备。"+
					"NPC 客户端以 root 权限运行在 Android 上，可直接控制 adbd 的启动/停止/重启，或一键重启手机。\n"+
					"注意：执行 stop 后 ADB 连接会断开，执行 reboot 后设备会重启"),
				mcp.WithNumber("client_id", mcp.Required(), mcp.Description("NPS 客户端 ID（整数）")),
				mcp.WithString("command", mcp.Required(), mcp.Description("控制命令：start（启动 adbd）、stop（停止 adbd）、restart（重启 adbd）、reboot（重启手机）")),
			),
			a.handleNPSAdbCtl,
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
// 通用 JSON 响应结构体和辅助方法
// ============================================================

// deviceInfo 表示 adb devices -l 输出中的单个设备
type deviceInfo struct {
	Serial      string `json:"serial"`
	State       string `json:"state"`
	Usb         string `json:"usb,omitempty"`
	Product     string `json:"product,omitempty"`
	Model       string `json:"model,omitempty"`
	Device      string `json:"device,omitempty"`
	TransportID string `json:"transport_id,omitempty"`
}

// devicesResponse 是 ADB 设备列表工具的返回结构
type devicesResponse struct {
	Devices []deviceInfo `json:"devices"`
	Count   int          `json:"count"`
}

// packagesResponse 表示应用列表工具的返回结构
type packagesResponse struct {
	Packages []string `json:"packages"`
	Count    int      `json:"count"`
}

// propertiesResponse 表示 getprop 返回全部属性时的结构
type propertiesResponse struct {
	Properties map[string]string `json:"properties"`
	Count      int               `json:"count"`
}

// singlePropertyResponse 表示 getprop 查询单个属性时的结构
type singlePropertyResponse struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// installResponse 表示安装应用结果
type installResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Package string `json:"package"`
}

// simpleActionResponse 表示通用成功/失败响应
type simpleActionResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// rebootResponse 表示重启设备结果
type rebootResponse struct {
	Success bool   `json:"success"`
	Serial  string `json:"serial"`
	Mode    string `json:"mode"`
}

// processInfo 表示 ps 输出中的单个进程
type processInfo struct {
	User string `json:"user"`
	PID  int    `json:"pid"`
	Name string `json:"name"`
}

// processesResponse 表示进程列表返回结构
type processesResponse struct {
	Processes []processInfo `json:"processes"`
	Count     int           `json:"count"`
}

// forwardInfo 表示单条端口转发记录
type forwardInfo struct {
	Serial string `json:"serial,omitempty"`
	Local  string `json:"local"`
	Remote string `json:"remote"`
}

// forwardsResponse 表示端口转发列表
type forwardsResponse struct {
	Forwards []forwardInfo `json:"forwards"`
	Count    int           `json:"count"`
}

// fileEntry 表示文件管理 ls 结果中的单个条目
type fileEntry struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // file/dir/link/other
	Size        string `json:"size,omitempty"`
	Permissions string `json:"permissions,omitempty"`
}

// fileListResponse 表示文件管理 ls 操作的结果
type fileListResponse struct {
	Entries []fileEntry `json:"entries"`
	Path    string      `json:"path"`
	Count   int         `json:"count"`
}

// npsClientSummary 表示 NPS 客户端简要信息
type npsClientSummary struct {
	ID      int    `json:"id"`
	Remark  string `json:"remark"`
	Addr    string `json:"addr"`
	Online  bool   `json:"online"`
	Version string `json:"version"`
}

// npsClientListResult 表示 NPS 设备列表工具的返回结构
type npsClientListResult struct {
	Clients []npsClientSummary `json:"clients"`
	Count   int                `json:"count"`
}

// npsPingResult 表示 NPS 连通测试结果
type npsPingResult struct {
	ClientID int  `json:"client_id"`
	Online   bool `json:"online"`
	RTTMs    int  `json:"rtt_ms"`
}

// npsTunnelSummary 表示单条 NPS 隧道摘要
type npsTunnelSummary struct {
	ID           int    `json:"id"`
	ClientID     int    `json:"client_id"`
	ClientRemark string `json:"client_remark"`
	ClientOnline bool   `json:"client_online"`
	Port         int    `json:"port"`
	Target       string `json:"target"`
}

// npsTunnelListResult 表示 NPS 隧道列表工具的返回结构
type npsTunnelListResult struct {
	Tunnels []npsTunnelSummary `json:"tunnels"`
	Count   int                `json:"count"`
}

// newJSONResult 将任意结构体编码为 JSON 字符串并返回 MCP 文本结果
func newJSONResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("JSON 编码失败: %v", err)), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// parseADBDevices 解析 adb devices -l 文本输出
func parseADBDevices(output string) devicesResponse {
	var devices []deviceInfo
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "List of devices attached") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		dev := deviceInfo{
			Serial: fields[0],
			State:  fields[1],
		}
		for _, f := range fields[2:] {
			kv := strings.SplitN(f, ":", 2)
			if len(kv) != 2 {
				continue
			}
			key := kv[0]
			val := kv[1]
			switch key {
			case "usb":
				dev.Usb = val
			case "product":
				dev.Product = val
			case "model":
				dev.Model = val
			case "device":
				dev.Device = val
			case "transport_id":
				dev.TransportID = val
			}
		}
		devices = append(devices, dev)
	}
	return devicesResponse{
		Devices: devices,
		Count:   len(devices),
	}
}

// parsePackages 解析 pm list packages 输出
func parsePackages(output string) packagesResponse {
	var pkgs []string
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "package:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "package:"))
		}
		if line != "" {
			pkgs = append(pkgs, line)
		}
	}
	return packagesResponse{
		Packages: pkgs,
		Count:    len(pkgs),
	}
}

// parseGetpropLine 解析 getprop 输出的单行 [key]: [value]
func parseGetpropLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", false
	}
	parts := strings.SplitN(line, "]: [", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	key := strings.TrimPrefix(parts[0], "[")
	value := strings.TrimSuffix(parts[1], "]")
	return key, value, true
}

// parseAllProperties 解析 getprop 全量输出
func parseAllProperties(output string) propertiesResponse {
	props := make(map[string]string)
	count := 0
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		key, value, ok := parseGetpropLine(line)
		if !ok {
			continue
		}
		props[key] = value
		count++
	}
	return propertiesResponse{
		Properties: props,
		Count:      count,
	}
}

// parseSingleProperty 解析 getprop 单个属性输出
func parseSingleProperty(output, prop string) singlePropertyResponse {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		key, value, ok := parseGetpropLine(line)
		if !ok {
			continue
		}
		return singlePropertyResponse{Key: key, Value: value}
	}
	// 兜底：返回原始输出
	return singlePropertyResponse{Key: prop, Value: strings.TrimSpace(output)}
}

// parseProcesses 解析 ps 输出
func parseProcesses(output, filter string) processesResponse {
	var processes []processInfo
	lines := strings.Split(output, "\n")
	isFirstLine := true
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if isFirstLine {
			// 第一行通常是表头，包含 PID 字段，直接跳过
			isFirstLine = false
			if strings.Contains(line, "PID") {
				continue
			}
		}
		if filter != "" && !strings.Contains(line, filter) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		user := fields[0]
		pidStr := fields[1]
		name := fields[len(fields)-1]
		pid, _ := strconv.Atoi(pidStr)
		processes = append(processes, processInfo{
			User: user,
			PID:  pid,
			Name: name,
		})
	}
	return processesResponse{
		Processes: processes,
		Count:     len(processes),
	}
}

// parseForwards 解析 adb forward --list 输出
func parseForwards(output string) forwardsResponse {
	var forwards []forwardInfo
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 3 {
			forwards = append(forwards, forwardInfo{
				Serial: fields[0],
				Local:  fields[1],
				Remote: fields[2],
			})
		} else if len(fields) == 2 {
			forwards = append(forwards, forwardInfo{
				Local:  fields[0],
				Remote: fields[1],
			})
		}
	}
	return forwardsResponse{
		Forwards: forwards,
		Count:    len(forwards),
	}
}

// parseFileList 解析 adb shell ls -l 输出
func parseFileList(path, output string) fileListResponse {
	var entries []fileEntry
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "total ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		perms := fields[0]
		var size string
		if len(fields) >= 5 {
			size = fields[4]
		}
		name := fields[len(fields)-1]
		fType := "other"
		if len(perms) > 0 {
			switch perms[0] {
			case 'd':
				fType = "dir"
			case '-':
				fType = "file"
			case 'l':
				fType = "link"
			}
		}
		entries = append(entries, fileEntry{
			Name:        name,
			Type:        fType,
			Size:        size,
			Permissions: perms,
		})
	}
	return fileListResponse{
		Entries: entries,
		Path:    path,
		Count:   len(entries),
	}
}

// ============================================================
// 11 个 ADB 工具 Handler（逻辑完全不变）
// ============================================================

// handleDevices 列出所有已连接的 ADB 设备。
// 当配置了 NPS 时，自动查询在线的远程设备并通过 adb connect 连接其 ADB 隧道，
// 使远程设备也出现在 adb devices 输出中
func (a *app) handleDevices(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if a.nps != nil {
		a.autoConnectNPSDevices(ctx)
	}
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()
	output, err := a.runADBText(callCtx, "devices", "-l")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp := parseADBDevices(output)
	return newJSONResult(resp)
}

// autoConnectNPSDevices 查询 NPS 在线客户端的 ADB 隧道，自动执行 adb connect
func (a *app) autoConnectNPSDevices(ctx context.Context) {
	body, err := a.nps.doPost("/client/list", "offset=0&limit=9999&search=&sort=&order=")
	if err != nil {
		return
	}
	var clientResp npsClientListResp
	if err := json.Unmarshal(body, &clientResp); err != nil {
		return
	}

	// 从 NPS baseURL 提取服务器 IP
	parsed, err := url.Parse(a.nps.baseURL)
	if err != nil {
		return
	}
	npsHost := parsed.Hostname()

	for _, c := range clientResp.Rows {
		if !c.IsConnect {
			continue
		}
		tunnels, err := a.fetchTunnels(c.Id, "")
		if err != nil || len(tunnels) == 0 {
			continue
		}
		// 取端口最小的隧道（ADB），端口+1 是 scrcpy
		minPort := tunnels[0].Port
		for _, t := range tunnels[1:] {
			if t.Port < minPort {
				minPort = t.Port
			}
		}
		addr := fmt.Sprintf("%s:%d", npsHost, minPort)
		// adb connect，忽略错误（可能已连接）
		connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		a.runADBText(connectCtx, "connect", addr)
		cancel()
	}
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

// uiNode 表示 uiautomator dump XML 中的一个 UI 节点
type uiNode struct {
	XMLName    xml.Name `xml:"node"`
	Text       string   `xml:"text,attr"`
	ResourceID string   `xml:"resource-id,attr"`
	Class      string   `xml:"class,attr"`
	Package    string   `xml:"package,attr"`
	Desc       string   `xml:"content-desc,attr"`
	Clickable  string   `xml:"clickable,attr"`
	Bounds     string   `xml:"bounds,attr"`
	Children   []uiNode `xml:"node"`
}

// uiHierarchy 表示 uiautomator dump 的根节点
type uiHierarchy struct {
	XMLName xml.Name `xml:"hierarchy"`
	Nodes   []uiNode `xml:"node"`
}

// uiElement 是返回给 AI 的扁平化 UI 元素
type uiElement struct {
	Text        string `json:"text,omitempty"`
	ResourceID  string `json:"resource_id,omitempty"`
	Class       string `json:"class"`
	ContentDesc string `json:"content_desc,omitempty"`
	Clickable   bool   `json:"clickable"`
	Bounds      string `json:"bounds"`
	CenterX     int    `json:"center_x"`
	CenterY     int    `json:"center_y"`
}

// flattenNodes 将嵌套的 UI 节点树递归展平为一维数组，过滤掉无意义的空节点
func flattenNodes(nodes []uiNode) []uiElement {
	var result []uiElement
	for _, n := range nodes {
		// 只保留有文字、有 resource-id、有 content-desc 或可点击的元素
		hasContent := n.Text != "" || n.ResourceID != "" || n.Desc != "" || n.Clickable == "true"
		if hasContent && n.Bounds != "" {
			cx, cy := parseBoundsCenter(n.Bounds)
			result = append(result, uiElement{
				Text:        n.Text,
				ResourceID:  n.ResourceID,
				Class:       n.Class,
				ContentDesc: n.Desc,
				Clickable:   n.Clickable == "true",
				Bounds:      n.Bounds,
				CenterX:     cx,
				CenterY:     cy,
			})
		}
		if len(n.Children) > 0 {
			result = append(result, flattenNodes(n.Children)...)
		}
	}
	return result
}

// parseBoundsCenter 从 "[x1,y1][x2,y2]" 格式解析中心坐标
func parseBoundsCenter(bounds string) (int, int) {
	// 格式: "[left,top][right,bottom]"
	bounds = strings.ReplaceAll(bounds, "][", ",")
	bounds = strings.Trim(bounds, "[]")
	parts := strings.Split(bounds, ",")
	if len(parts) != 4 {
		return 0, 0
	}
	x1, _ := strconv.Atoi(parts[0])
	y1, _ := strconv.Atoi(parts[1])
	x2, _ := strconv.Atoi(parts[2])
	y2, _ := strconv.Atoi(parts[3])
	return (x1 + x2) / 2, (y1 + y2) / 2
}

// handleUILayout 通过 uiautomator dump 获取设备当前界面的 UI 元素树
// 返回扁平化的 JSON 数组，每个元素包含文字、坐标中心点、类型、可点击性
func (a *app) handleUILayout(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()

	// Step 1: 执行 uiautomator dump 到设备临时文件
	dumpPath := "/sdcard/ui_dump.xml"
	dumpArgs := append(serialArgs(serial), "shell", "uiautomator", "dump", dumpPath)
	_, err = a.runADBText(callCtx, dumpArgs...)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("uiautomator dump 失败：%v", err)), nil
	}

	// Step 2: 读取 XML 内容
	catArgs := append(serialArgs(serial), "shell", "cat", dumpPath)
	xmlOutput, err := a.runADBText(callCtx, catArgs...)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("读取 UI dump 失败：%v", err)), nil
	}

	// Step 3: 清理临时文件
	rmArgs := append(serialArgs(serial), "shell", "rm", "-f", dumpPath)
	_, _ = a.runADBText(callCtx, rmArgs...)

	// Step 4: 解析 XML
	var hierarchy uiHierarchy
	if err := xml.Unmarshal([]byte(xmlOutput), &hierarchy); err != nil {
		// XML 解析失败时返回原始内容，让 AI 自行处理
		return mcp.NewToolResultText(xmlOutput), nil
	}

	// Step 5: 展平并过滤，返回 JSON
	elements := flattenNodes(hierarchy.Nodes)
	resp := map[string]interface{}{
		"count":    len(elements),
		"elements": elements,
	}
	data, _ := json.MarshalIndent(resp, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

// handleLogcat 获取设备 logcat 日志
// 支持三种模式：
//   - 按行数（默认）：-d -t N
//   - 按时间（since 参数）：-d -T 'timestamp'，纯数字会转为「当前时间 - N秒」
//   - 配合清空日志使用：先 logcat -c，再调用本工具获取新日志
func (a *app) handleLogcat(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	since := req.GetString("since", "")
	filter := req.GetString("filter", "")
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()

	// 构建 logcat 参数：-d 确保非阻塞
	args := serialArgs(serial)
	args = append(args, "logcat", "-d")

	if since != "" {
		// since 参数处理：纯数字视为「最近 N 秒」，否则作为绝对时间戳传给 -T
		var timestamp string
		if n, parseErr := strconv.Atoi(strings.TrimSpace(since)); parseErr == nil && n > 0 {
			// 纯数字：计算 N 秒前的时间戳，格式为 logcat -T 要求的 'MM-DD HH:MM:SS.mmm'
			t := time.Now().Add(-time.Duration(n) * time.Second)
			timestamp = t.Format("01-02 15:04:05.000")
		} else {
			timestamp = strings.TrimSpace(since)
		}
		args = append(args, "-T", timestamp)
	} else {
		// 默认按行数
		lines := req.GetInt("lines", 50)
		if lines <= 0 {
			lines = 50
		}
		args = append(args, "-t", fmt.Sprintf("%d", lines))
	}

	if filter != "" {
		args = append(args, strings.Fields(filter)...)
	}
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(output), nil
}

// handleLogcatClear 清空设备 logcat 缓冲区
// AI 工作流：清空日志 → 执行操作 → 查看日志（只看新产生的）
func (a *app) handleLogcatClear(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()
	args := append(serialArgs(serial), "logcat", "-c")
	_, err = a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	result := map[string]interface{}{
		"success": true,
		"message": "logcat 缓冲区已清空",
		"serial":  serial,
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
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
	resp := parsePackages(output)
	return newJSONResult(resp)
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
	pkgName := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	resp := installResponse{
		Success: true,
		Message: strings.TrimSpace(output),
		Package: pkgName,
	}
	return newJSONResult(resp)
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
	if prop != "" {
		resp := parseSingleProperty(output, prop)
		return newJSONResult(resp)
	}
	resp := parseAllProperties(output)
	return newJSONResult(resp)
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
	_, err = a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	normalizedMode := mode
	if normalizedMode == "" {
		normalizedMode = "normal"
	}
	resp := rebootResponse{
		Success: true,
		Serial:  serial,
		Mode:    normalizedMode,
	}
	return newJSONResult(resp)
}

// handlePush 将本地文件推送到设备
func (a *app) handlePush(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	localPath, err := req.RequireString("local_path")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	remotePath, err := req.RequireString("remote_path")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, defaultInstallTimeout)
	defer cancel()
	args := append(serialArgs(serial), "push", localPath, remotePath)
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp := simpleActionResponse{
		Success: true,
		Message: strings.TrimSpace(output),
	}
	return newJSONResult(resp)
}

// handlePull 将设备上的文件拉取到本机
func (a *app) handlePull(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	remotePath, err := req.RequireString("remote_path")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	localPath, err := req.RequireString("local_path")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, defaultInstallTimeout)
	defer cancel()
	args := append(serialArgs(serial), "pull", remotePath, localPath)
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp := simpleActionResponse{
		Success: true,
		Message: strings.TrimSpace(output),
	}
	return newJSONResult(resp)
}

// handleUninstall 卸载设备上的应用
func (a *app) handleUninstall(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	pkg, err := req.RequireString("package")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	keepData := req.GetBool("keep_data", false)
	callCtx, cancel := context.WithTimeout(ctx, defaultInstallTimeout)
	defer cancel()
	args := append(serialArgs(serial), "uninstall")
	if keepData {
		args = append(args, "-k")
	}
	args = append(args, pkg)
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp := simpleActionResponse{
		Success: true,
		Message: strings.TrimSpace(output),
	}
	return newJSONResult(resp)
}

// handleStartActivity 启动应用 Activity 或默认入口
func (a *app) handleStartActivity(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	pkg, err := req.RequireString("package")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	activity := req.GetString("activity", "")
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()
	var args []string
	if strings.TrimSpace(activity) == "" {
		// 未指定 Activity 时使用 monkey 启动默认入口 Activity
		args = append(serialArgs(serial), "shell", "monkey", "-p", pkg, "1")
	} else {
		component := activity
		if !strings.Contains(activity, "/") {
			component = pkg + "/" + activity
		}
		args = append(serialArgs(serial), "shell", "am", "start", "-n", component)
	}
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp := simpleActionResponse{
		Success: true,
		Message: strings.TrimSpace(output),
	}
	return newJSONResult(resp)
}

// handleForceStop 停止应用进程
func (a *app) handleForceStop(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	pkg, err := req.RequireString("package")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()
	args := append(serialArgs(serial), "shell", "am", "force-stop", pkg)
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp := simpleActionResponse{
		Success: true,
		Message: strings.TrimSpace(output),
	}
	return newJSONResult(resp)
}

// handleClearData 清除应用数据
func (a *app) handleClearData(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	pkg, err := req.RequireString("package")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()
	args := append(serialArgs(serial), "shell", "pm", "clear", pkg)
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp := simpleActionResponse{
		Success: true,
		Message: strings.TrimSpace(output),
	}
	return newJSONResult(resp)
}

// handleInput 在设备上执行 input 输入操作
func (a *app) handleInput(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	action, err := req.RequireString("action")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	argsStr, err := req.RequireString("args")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	action = strings.ToLower(strings.TrimSpace(action))
	baseArgs := append(serialArgs(serial), "shell", "input")
	switch action {
	case "tap", "swipe", "keyevent":
		parts := strings.Fields(argsStr)
		if len(parts) == 0 {
			return mcp.NewToolResultError("args 不能为空"), nil
		}
		baseArgs = append(baseArgs, action)
		baseArgs = append(baseArgs, parts...)
	case "text":
		baseArgs = append(baseArgs, "text", argsStr)
	default:
		return mcp.NewToolResultError(fmt.Sprintf("不支持的输入动作：%q，可选值：tap/swipe/text/keyevent", action)), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()
	output, err := a.runADBText(callCtx, baseArgs...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp := simpleActionResponse{
		Success: true,
		Message: strings.TrimSpace(output),
	}
	return newJSONResult(resp)
}

// handleForward 管理端口转发
func (a *app) handleForward(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	action, err := req.RequireString("action")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	local := req.GetString("local", "")
	remote := req.GetString("remote", "")
	action = strings.ToLower(strings.TrimSpace(action))
	switch action {
	case "list":
		callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
		defer cancel()
		args := append(serialArgs(serial), "forward", "--list")
		output, err := a.runADBText(callCtx, args...)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		resp := parseForwards(output)
		return newJSONResult(resp)
	case "add":
		if local == "" || remote == "" {
			return mcp.NewToolResultError("local 和 remote 均不能为空"), nil
		}
		callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
		defer cancel()
		args := append(serialArgs(serial), "forward", local, remote)
		output, err := a.runADBText(callCtx, args...)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		resp := simpleActionResponse{
			Success: true,
			Message: strings.TrimSpace(output),
		}
		return newJSONResult(resp)
	case "remove":
		if local == "" {
			return mcp.NewToolResultError("local 不能为空"), nil
		}
		callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
		defer cancel()
		args := append(serialArgs(serial), "forward", "--remove", local)
		output, err := a.runADBText(callCtx, args...)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		resp := simpleActionResponse{
			Success: true,
			Message: strings.TrimSpace(output),
		}
		return newJSONResult(resp)
	default:
		return mcp.NewToolResultError(fmt.Sprintf("不支持的 action：%q，可选值：list/add/remove", action)), nil
	}
}

// handleDumpsys 获取系统服务信息
func (a *app) handleDumpsys(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	service, err := req.RequireString("service")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()
	args := append(serialArgs(serial), "shell", "dumpsys")
	service = strings.TrimSpace(service)
	if service != "" {
		args = append(args, strings.Fields(service)...)
	}
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	result := map[string]any{
		"service": service,
		"output":  output,
	}
	return newJSONResult(result)
}

// handleFileOps 执行基础文件管理操作
func (a *app) handleFileOps(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	action, err := req.RequireString("action")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	extra := req.GetString("extra", "")
	action = strings.ToLower(strings.TrimSpace(action))
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()
	var args []string
	switch action {
	case "ls":
		args = append(serialArgs(serial), "shell", "ls", "-l", path)
		output, err := a.runADBText(callCtx, args...)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		resp := parseFileList(path, output)
		return newJSONResult(resp)
	case "cat":
		args = append(serialArgs(serial), "shell", "cat", path)
		output, err := a.runADBText(callCtx, args...)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		result := map[string]any{
			"path":    path,
			"content": output,
		}
		return newJSONResult(result)
	case "rm":
		args = append(serialArgs(serial), "shell", "rm", path)
	case "mkdir":
		args = append(serialArgs(serial), "shell", "mkdir", "-p", path)
	case "chmod":
		if extra == "" {
			return mcp.NewToolResultError("chmod 操作需要提供 extra（权限模式，例如 644）"), nil
		}
		args = append(serialArgs(serial), "shell", "chmod", extra, path)
	case "stat":
		args = append(serialArgs(serial), "shell", "stat", path)
	default:
		return mcp.NewToolResultError(fmt.Sprintf("不支持的文件操作：%q，可选值：ls/cat/rm/mkdir/chmod/stat", action)), nil
	}
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if action == "stat" {
		result := map[string]any{
			"path": path,
			"stat": output,
		}
		return newJSONResult(result)
	}
	resp := simpleActionResponse{
		Success: true,
		Message: strings.TrimSpace(output),
	}
	return newJSONResult(resp)
}

// handlePs 获取进程列表
func (a *app) handlePs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	filter := req.GetString("filter", "")
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()
	args := append(serialArgs(serial), "shell", "ps")
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp := parseProcesses(output, filter)
	return newJSONResult(resp)
}

// handleRoot 切换 ADB root/unroot 模式
func (a *app) handleRoot(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	enable, err := req.RequireBool("enable")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, a.shellTimeout)
	defer cancel()
	cmd := "unroot"
	if enable {
		cmd = "root"
	}
	args := append(serialArgs(serial), cmd)
	output, err := a.runADBText(callCtx, args...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	message := strings.TrimSpace(output)
	if message == "" {
		if enable {
			message = "已尝试切换到 root 模式"
		} else {
			message = "已尝试关闭 root 模式"
		}
	}
	resp := simpleActionResponse{
		Success: true,
		Message: message,
	}
	return newJSONResult(resp)
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
	clients := make([]npsClientSummary, 0, len(resp.Rows))
	for _, c := range resp.Rows {
		clients = append(clients, npsClientSummary{
			ID:      c.Id,
			Remark:  c.Remark,
			Addr:    c.Addr,
			Online:  c.IsConnect,
			Version: c.Version,
		})
	}
	result := npsClientListResult{
		Clients: clients,
		Count:   resp.Total,
	}
	return newJSONResult(result)
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
	result := npsPingResult{
		ClientID: clientID,
		Online:   resp.Code == 1,
		RTTMs:    resp.RTT,
	}
	return newJSONResult(result)
}

// handleNPSTunnelList 查询 NPS 服务器上的隧道列表，支持按客户端 ID 和类型筛选。
// NPS 后端的 GetTunnel 在 client_id=0 时不返回任何数据（过滤逻辑要求精确匹配），
// 因此当未指定 client_id 时，先查询所有客户端，再逐个客户端查询隧道并合并结果
func (a *app) handleNPSTunnelList(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	clientID := req.GetInt("client_id", 0)
	tunnelType := req.GetString("type", "")

	// 先查所有客户端，获取在线状态和备注
	body, err := a.nps.doPost("/client/list", "offset=0&limit=9999&search=&sort=&order=")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("查询客户端列表失败: %v", err)), nil
	}
	var clientResp npsClientListResp
	if err := json.Unmarshal(body, &clientResp); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("解析客户端响应失败: %v", err)), nil
	}
	clientMap := make(map[int]npsClientInfo)
	for _, c := range clientResp.Rows {
		clientMap[c.Id] = c
	}

	var allTunnels []npsTunnelInfo
	if clientID != 0 {
		tunnels, err := a.fetchTunnels(clientID, tunnelType)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("查询隧道列表失败: %v", err)), nil
		}
		allTunnels = tunnels
	} else {
		for _, c := range clientResp.Rows {
			tunnels, err := a.fetchTunnels(c.Id, tunnelType)
			if err != nil {
				continue
			}
			allTunnels = append(allTunnels, tunnels...)
		}
	}

	// 回填客户端备注和在线状态
	for i := range allTunnels {
		if c, ok := clientMap[allTunnels[i].ClientID]; ok {
			allTunnels[i].ClientRemark = c.Remark
			allTunnels[i].ClientOnline = c.IsConnect
		}
	}

	// 每个客户端有两条隧道：端口小的是 ADB，端口大的（+1）是 scrcpy，只保留 ADB 隧道
	adbPortByClient := make(map[int]int)
	for _, t := range allTunnels {
		if p, ok := adbPortByClient[t.ClientID]; !ok || t.Port < p {
			adbPortByClient[t.ClientID] = t.Port
		}
	}
	var adbTunnels []npsTunnelInfo
	for _, t := range allTunnels {
		if adbPortByClient[t.ClientID] == t.Port {
			adbTunnels = append(adbTunnels, t)
		}
	}

	if len(adbTunnels) == 0 {
		result := npsTunnelListResult{
			Tunnels: []npsTunnelSummary{},
			Count:   0,
		}
		return newJSONResult(result)
	}
	resultTunnels := make([]npsTunnelSummary, 0, len(adbTunnels))
	for _, t := range adbTunnels {
		resultTunnels = append(resultTunnels, npsTunnelSummary{
			ID:           t.Id,
			ClientID:     t.ClientID,
			ClientRemark: t.ClientRemark,
			ClientOnline: t.ClientOnline,
			Port:         t.Port,
			Target:       t.Target.TargetStr,
		})
	}
	result := npsTunnelListResult{
		Tunnels: resultTunnels,
		Count:   len(resultTunnels),
	}
	return newJSONResult(result)
}

// fetchTunnels 查询指定客户端的隧道列表
func (a *app) fetchTunnels(clientID int, tunnelType string) ([]npsTunnelInfo, error) {
	params := fmt.Sprintf("offset=0&limit=9999&client_id=%d&type=%s&search=&sort=&order=", clientID, tunnelType)
	body, err := a.nps.doPost("/index/gettunnel", params)
	if err != nil {
		return nil, err
	}
	var resp npsTunnelListResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	// 回填 ClientID（API 响应里嵌套在 Client 对象中，这里简化处理）
	for i := range resp.Rows {
		resp.Rows[i].ClientID = clientID
	}
	return resp.Rows, nil
}

// handleNPSAdbCtl 通过 NPS 隧道远程控制 NPC 设备上的 adbd 服务。
// 支持三种命令：start（启动 adbd）、stop（停止 adbd）、restart（重启 adbd）。
// 通过 NPS Web API /client/adbctl 发送命令到 NPC 客户端，NPC 在本地执行后返回结果。
func (a *app) handleNPSAdbCtl(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if a.nps == nil {
		return mcp.NewToolResultError("未配置 NPS，无法使用远程 ADB 控制"), nil
	}
	clientID, err := req.RequireInt("client_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	command, err := req.RequireString("command")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	// 校验命令合法性
	switch command {
	case "start", "stop", "restart", "reboot":
	default:
		return mcp.NewToolResultError(fmt.Sprintf("不支持的命令：%q，可选值：start/stop/restart/reboot", command)), nil
	}
	body, err := a.nps.doPost("/client/adbctl", fmt.Sprintf("id=%d&command=%s", clientID, command))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("远程 ADB 控制失败: %v", err)), nil
	}
	var resp npsAdbCtlResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("解析 NPS 响应失败: %v", err)), nil
	}
	return newJSONResult(resp)
}
