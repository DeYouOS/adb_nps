// adb.go 提供通过 go-adb-kit 操作 ADB 服务的 MCP 工具集。
// 所有工具均连接至 mcp_adb_addr 指定的 ADB 服务（由 NPS 隧道转发自 NPC 侧），
// 每次调用均建立新连接（因 Transport 选择后连接不可复用）。
package mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/airhandsome/go-adb-kit/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// adbAddr 存储 ADB 服务器地址（由 server.go 中 Start() 从配置读取后赋值）。
// 默认值为 "127.0.0.1:5037"，对应本地 ADB 服务或 NPS 隧道转发端口。
var adbAddr = "127.0.0.1:5037"

// adbShellOutputLimit 限制 shell 命令输出最大字节数（64 KB），
// 防止超大输出导致 MCP 消息过大。
const adbShellOutputLimit = 64 * 1024

// adbShellTimeout 是执行单条 shell 命令的超时时间（30 秒）。
const adbShellTimeout = 30 * time.Second

// registerADBTools 向 MCP 服务器注册所有 ADB 相关工具。
func registerADBTools(s *server.MCPServer) {
	s.AddTool(
		mcp.NewTool("ADB设备列表",
			mcp.WithDescription("列出 ADB 服务器上所有已连接设备（等同于 adb devices -l）"),
		),
		handleADBDevices,
	)

	s.AddTool(
		mcp.NewTool("执行命令",
			mcp.WithDescription("在指定 ADB 设备上执行 shell 命令，返回命令输出（最多 64 KB）"),
			mcp.WithString("serial",
				mcp.Required(),
				mcp.Description("目标设备序列号（adb devices 输出的第一列）"),
			),
			mcp.WithString("command",
				mcp.Required(),
				mcp.Description("要执行的 shell 命令，例如：ls /data/local/tmp"),
			),
		),
		handleADBShell,
	)

	s.AddTool(
		mcp.NewTool("截屏",
			mcp.WithDescription("截取指定 ADB 设备的屏幕截图，以 base64 编码的 PNG 图片形式返回"),
			mcp.WithString("serial",
				mcp.Required(),
				mcp.Description("目标设备序列号"),
			),
		),
		handleADBScreencap,
	)

	s.AddTool(
		mcp.NewTool("查看日志",
			mcp.WithDescription("从指定 ADB 设备获取 logcat 日志，支持行数限制和过滤器"),
			mcp.WithString("serial",
				mcp.Required(),
				mcp.Description("目标设备序列号"),
			),
			mcp.WithNumber("lines",
				mcp.Description("返回的日志行数，默认 50"),
				mcp.DefaultNumber(50),
			),
			mcp.WithString("filter",
				mcp.Description("logcat 过滤器表达式，例如：ActivityManager:I *:S（留空不过滤）"),
			),
		),
		handleADBLogcat,
	)

	s.AddTool(
		mcp.NewTool("应用列表",
			mcp.WithDescription("列出指定 ADB 设备上已安装的应用包名列表"),
			mcp.WithString("serial",
				mcp.Required(),
				mcp.Description("目标设备序列号"),
			),
			mcp.WithString("filter",
				mcp.Description("包名关键词过滤，留空返回所有包"),
			),
		),
		handleADBPackages,
	)

	s.AddTool(
		mcp.NewTool("安装应用",
			mcp.WithDescription("在指定 ADB 设备上安装 APK（通过 pm install 命令，路径须为设备侧路径）"),
			mcp.WithString("serial",
				mcp.Required(),
				mcp.Description("目标设备序列号"),
			),
			mcp.WithString("path",
				mcp.Required(),
				mcp.Description("APK 在设备侧的路径，例如：/data/local/tmp/app.apk"),
			),
		),
		handleADBInstall,
	)

	s.AddTool(
		mcp.NewTool("推送文件",
			mcp.WithDescription("推送本地文件至 ADB 设备（当前通过隧道暂不支持，返回占位提示）"),
			mcp.WithString("serial",
				mcp.Required(),
				mcp.Description("目标设备序列号"),
			),
			mcp.WithString("local",
				mcp.Required(),
				mcp.Description("本地文件路径"),
			),
			mcp.WithString("remote",
				mcp.Required(),
				mcp.Description("设备目标路径"),
			),
		),
		handleADBPush,
	)

	s.AddTool(
		mcp.NewTool("拉取文件",
			mcp.WithDescription("从 ADB 设备拉取文件至本地（当前通过隧道暂不支持，返回占位提示）"),
			mcp.WithString("serial",
				mcp.Required(),
				mcp.Description("目标设备序列号"),
			),
			mcp.WithString("remote",
				mcp.Required(),
				mcp.Description("设备源文件路径"),
			),
			mcp.WithString("local",
				mcp.Required(),
				mcp.Description("本地目标路径"),
			),
		),
		handleADBPull,
	)

	s.AddTool(
		mcp.NewTool("系统属性",
			mcp.WithDescription("读取指定 ADB 设备的 Android 系统属性，可指定单个属性名或不传返回全部"),
			mcp.WithString("serial",
				mcp.Required(),
				mcp.Description("目标设备序列号"),
			),
			mcp.WithString("prop",
				mcp.Description("属性名称，例如：ro.build.version.release（留空返回所有属性）"),
			),
		),
		handleADBGetprop,
	)

	s.AddTool(
		mcp.NewTool("重启设备",
			mcp.WithDescription("重启指定 ADB 设备，可选进入 recovery 或 bootloader 模式"),
			mcp.WithString("serial",
				mcp.Required(),
				mcp.Description("目标设备序列号"),
			),
			mcp.WithString("mode",
				mcp.Description("重启模式：留空为正常重启，recovery 为进入 recovery，bootloader 为进入 bootloader"),
			),
		),
		handleADBReboot,
	)
}

// dialADB 建立到 ADB 服务器的新 TCP 连接。
// 每次 shell/transport 操作后连接均不可复用，故每次工具调用均新建连接。
func dialADB(ctx context.Context) (*transport.Conn, error) {
	conn, err := transport.Dial(ctx, transport.DialOptions{
		Address:     adbAddr,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("连接 ADB 服务器 %s 失败: %w", adbAddr, err)
	}
	return conn, nil
}

// runShell 在指定设备上执行 shell 命令，读取并返回输出文本。
// 输出超过 adbShellOutputLimit（64 KB）时截断，并附加截断提示。
// ctx 控制整体超时（建议 30 秒）。
func runShell(ctx context.Context, serial string, args []string) (string, error) {
	conn, err := dialADB(ctx)
	if err != nil {
		return "", err
	}
	// Shell 内部会调用 Transport(serial)，连接之后不可复用，无需手动关闭
	reader, err := conn.Shell(ctx, serial, args)
	if err != nil {
		_ = conn.Close()
		return "", fmt.Errorf("执行 shell 命令失败: %w", err)
	}
	defer func() {
		_ = reader.Close()
		_ = conn.Close()
	}()

	// 读取输出，最多 adbShellOutputLimit + 1 字节（+1 用于判断是否超限）
	buf := make([]byte, adbShellOutputLimit+1)
	n, readErr := io.ReadAtLeast(reader, buf, 0)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		// 部分读取错误：上下文超时或设备断连
		if n == 0 {
			return "", fmt.Errorf("读取 shell 输出失败: %w", readErr)
		}
	}

	output := string(buf[:n])
	if n > adbShellOutputLimit {
		// 截断超大输出，避免 MCP 消息体过大
		output = output[:adbShellOutputLimit] + "\n[输出已截断，超过 64 KB 限制]"
	}
	return output, nil
}

// handleADBDevices 处理 adb_devices 工具调用，返回设备列表文本。
func handleADBDevices(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	conn, err := dialADB(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer func() { _ = conn.Close() }()

	// host:devices-l 等同于 adb devices -l，返回带详细信息的设备列表
	data, err := conn.HostRequest("devices-l")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("获取设备列表失败: %v", err)), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// handleADBShell 处理 adb_shell 工具调用，在目标设备上执行指定命令并返回输出。
func handleADBShell(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	command, err := req.RequireString("command")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}

	// 带超时 context，防止命令长时间阻塞
	shellCtx, cancel := context.WithTimeout(ctx, adbShellTimeout)
	defer cancel()

	// 将命令字符串按空格拆分为参数列表（ADB shell 协议需要）
	args := strings.Fields(command)
	if len(args) == 0 {
		return mcp.NewToolResultError("command 不能为空"), nil
	}

	output, err := runShell(shellCtx, serial, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(output), nil
}

// handleADBScreencap 处理 adb_screencap 工具调用，截屏并以 base64 PNG 图片返回。
func handleADBScreencap(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}

	// screencap -p 直接输出 PNG 字节流到 stdout
	screencapCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conn, err := dialADB(screencapCtx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	reader, err := conn.Shell(screencapCtx, serial, []string{"screencap", "-p"})
	if err != nil {
		_ = conn.Close()
		return mcp.NewToolResultError(fmt.Sprintf("截屏失败: %v", err)), nil
	}
	defer func() {
		_ = reader.Close()
		_ = conn.Close()
	}()

	// 读取全部 PNG 字节（截图通常几百 KB，不超过 10 MB 保险上限）
	const maxPNGSize = 10 * 1024 * 1024
	pngData, readErr := io.ReadAll(io.LimitReader(reader, maxPNGSize))
	if readErr != nil && readErr != io.EOF {
		return mcp.NewToolResultError(fmt.Sprintf("读取截图数据失败: %v", readErr)), nil
	}
	if len(pngData) == 0 {
		return mcp.NewToolResultError("截图数据为空，设备可能不支持 screencap"), nil
	}

	// 部分 Android 设备 screencap 输出包含 Windows 换行符（\r\n），需去掉 \r
	pngData = fixScreencapOutput(pngData)

	// 以 base64 PNG 图片形式返回，MCP 标准 ImageContent 格式
	// NewToolResultImage 签名：(text, imageData, mimeType)，text 为可读描述
	b64 := base64.StdEncoding.EncodeToString(pngData)
	return mcp.NewToolResultImage("设备屏幕截图", b64, "image/png"), nil
}

// fixScreencapOutput 移除 screencap 输出中可能存在的 \r 字节（Windows 换行兼容处理）。
// 某些设备的 screencap 通过 legacy shell 传输时会在 \n 前插入 \r，破坏 PNG 格式。
func fixScreencapOutput(data []byte) []byte {
	// 检查是否包含 \r\n，若无则直接返回原始数据，避免不必要的内存分配
	if !strings.Contains(string(data[:min(len(data), 1024)]), "\r") {
		return data
	}
	result := make([]byte, 0, len(data))
	for _, b := range data {
		if b != '\r' {
			result = append(result, b)
		}
	}
	return result
}

// min 返回两个整数中的较小值（兼容 Go 1.20 以下版本无内置 min 的情况）。
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// handleADBLogcat 处理 adb_logcat 工具调用，返回指定行数的 logcat 日志。
func handleADBLogcat(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}

	lines := req.GetInt("lines", 50)
	if lines <= 0 {
		lines = 50
	}
	filter := req.GetString("filter", "")

	// 构造 logcat 命令：-d 读取后退出，-t <lines> 限制行数
	args := []string{"logcat", "-d", fmt.Sprintf("-t %d", lines)}
	if filter != "" {
		// 追加过滤器表达式（例如 "ActivityManager:I *:S"）
		args = append(args, strings.Fields(filter)...)
	}

	shellCtx, cancel := context.WithTimeout(ctx, adbShellTimeout)
	defer cancel()

	output, err := runShell(shellCtx, serial, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(output), nil
}

// handleADBPackages 处理 adb_packages 工具调用，返回已安装包名列表。
func handleADBPackages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	filter := req.GetString("filter", "")

	args := []string{"pm", "list", "packages"}
	if filter != "" {
		args = append(args, filter)
	}

	shellCtx, cancel := context.WithTimeout(ctx, adbShellTimeout)
	defer cancel()

	output, err := runShell(shellCtx, serial, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(output), nil
}

// handleADBInstall 处理 adb_install 工具调用，通过 pm install 安装 APK。
// 要求 APK 文件已位于设备侧（例如通过其他途径 push 至 /data/local/tmp/）。
func handleADBInstall(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}

	// 使用 pm install -r（允许重装）执行安装
	args := []string{"pm", "install", "-r", path}

	// 安装操作允许较长超时（最多 2 分钟）
	installCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	output, err := runShell(installCtx, serial, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(output), nil
}

// handleADBPush 是 adb_push 的占位实现。
// 文件推送需要 sync 协议支持（SEND/DATA/DONE 帧），当前 go-adb-kit 通过隧道暂不支持，
// 留待后续实现。
func handleADBPush(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("adb_push 通过 NPS 隧道暂未实现，请通过其他方式传输文件"), nil
}

// handleADBPull 是 adb_pull 的占位实现。
// 文件拉取同样依赖 sync 协议，当前暂不支持。
func handleADBPull(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("adb_pull 通过 NPS 隧道暂未实现，请通过其他方式获取文件"), nil
}

// handleADBGetprop 处理 adb_getprop 工具调用，读取设备 Android 属性。
func handleADBGetprop(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	prop := req.GetString("prop", "")

	var args []string
	if prop != "" {
		args = []string{"getprop", prop}
	} else {
		// 不指定属性名时返回全部属性
		args = []string{"getprop"}
	}

	shellCtx, cancel := context.WithTimeout(ctx, adbShellTimeout)
	defer cancel()

	output, err := runShell(shellCtx, serial, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(output), nil
}

// handleADBReboot 处理 adb_reboot 工具调用，重启设备。
// 支持普通重启（mode=""）、进入 recovery（mode="recovery"）和进入 bootloader（mode="bootloader"）。
func handleADBReboot(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serial, err := req.RequireString("serial")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("参数错误：%v", err)), nil
	}
	mode := req.GetString("mode", "")

	var args []string
	switch mode {
	case "", "normal":
		args = []string{"reboot"}
	case "recovery":
		args = []string{"reboot", "recovery"}
	case "bootloader":
		args = []string{"reboot", "bootloader"}
	default:
		return mcp.NewToolResultError(
			fmt.Sprintf("不支持的重启模式：%q，可选值：空（正常重启）、recovery、bootloader", mode),
		), nil
	}

	// 重启命令发送后设备立即断连，超时设短一些避免无谓等待
	rebootCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// 重启命令通常不返回任何输出，runShell 返回 EOF 即正常
	_, _ = runShell(rebootCtx, serial, args)

	return mcp.NewToolResultText(
		fmt.Sprintf("已向设备 %s 发送重启命令（模式：%s）", serial, modeLabel(mode)),
	), nil
}

// modeLabel 将重启模式枚举转为可读标签，用于日志和返回消息。
func modeLabel(mode string) string {
	switch mode {
	case "recovery":
		return "recovery 模式"
	case "bootloader":
		return "bootloader 模式"
	default:
		return "正常重启"
	}
}
