//go:build !sdk
// +build !sdk

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/djylb/nps/client"
	"github.com/djylb/nps/lib/common"
	"github.com/djylb/nps/lib/logs"
	"github.com/djylb/nps/lib/version"

	goflag "flag"
	flag "github.com/spf13/pflag"
)

// 这里保留 Android 启动场景真正需要的最小参数集合。
var (
	ver            = flag.BoolP("version", "v", false, "Show current version")
	serverAddr     = flag.StringP("server", "s", "", "Server addr (ip1:port1,ip2:port2)")
	verifyKey      = flag.StringP("vkey", "k", "", "Authentication key (eg: vkey1,vkey2)")
	connType       = flag.StringP("type", "t", "tcp", "Connection type with the server (tcp|tls) (eg: tcp,tls)")
	logType        = flag.String("log", "file", "Log output mode (stdout|file|both|off)")
	logLevel       = flag.String("log_level", "trace", "Log level (trace|debug|info|warn|error|fatal|panic|off)")
	logPath        = flag.String("log_path", "", "NPC log path (empty to use default, 'off' to disable)")
	logMaxSize     = flag.Int("log_max_size", 5, "Maximum log file size in MB before rotation (0 to disable)")
	logMaxDays     = flag.Int("log_max_days", 7, "Number of days to retain old log files (0 to disable)")
	logMaxFiles    = flag.Int("log_max_files", 10, "Maximum number of log files to retain (0 to disable)")
	logCompress    = flag.Bool("log_compress", false, "Compress rotated log files (true or false)")
	logColor       = flag.Bool("log_color", true, "Enable ANSI color codes in console output (true or false)")
	debug          = flag.Bool("debug", true, "Enable debug mode")
	protoVer       = flag.Int("proto_version", version.GetLatestIndex(), fmt.Sprintf("Protocol version (0-%d)", version.GetLatestIndex()))
	skipVerify     = flag.Bool("skip_verify", false, "Skip verification of server certificate")
	disconnectTime = flag.Int("disconnect_timeout", 30, "Disconnect timeout in seconds")
	autoReconnect  = flag.Bool("auto_reconnect", true, "Auto Reconnect")
)

func main() {
	flag.CommandLine.SetNormalizeFunc(func(f *flag.FlagSet, name string) flag.NormalizedName {
		name = strings.ReplaceAll(name, "-", "_")
		name = strings.ReplaceAll(name, ".", "_")
		return flag.NormalizedName(name)
	})
	normalizeLegacyLongFlags()
	flag.CommandLine.SortFlags = false
	flag.CommandLine.SetInterspersed(true)
	flag.CommandLine.AddGoFlagSet(goflag.CommandLine)
	flag.Parse()

	if *ver {
		version.PrintVersion(*protoVer)
		return
	}
	if err := validateFlags(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	client.Ver = *protoVer
	client.SkipTLSVerify = *skipVerify
	configureLogging()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run(ctx, cancel)

	var wg sync.WaitGroup
	wg.Add(1)
	wg.Wait()
}

func normalizeLegacyLongFlags() {
	norm := func(s string) string {
		s = strings.ReplaceAll(s, "-", "_")
		s = strings.ReplaceAll(s, ".", "_")
		return s
	}
	defined := map[string]struct{}{}
	flag.CommandLine.VisitAll(func(f *flag.Flag) {
		defined[norm(f.Name)] = struct{}{}
	})
	if len(os.Args) <= 1 {
		return
	}
	out := make([]string, 0, len(os.Args))
	out = append(out, os.Args[0])
	for _, a := range os.Args[1:] {
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && len(a) > 2 {
			s := a[1:]
			name, val := s, ""
			if i := strings.IndexByte(s, '='); i >= 0 {
				name, val = s[:i], s[i:]
			}
			if _, ok := defined[norm(name)]; ok {
				a = "--" + name + val
			}
		}
		out = append(out, a)
	}
	os.Args = out
}

// validateFlags 约束精简版只接受 tcp/tls 两种到服务端的传输方式。
func validateFlags() error {
	allowed := map[string]struct{}{"tcp": {}, "tls": {}}
	for _, tp := range strings.Split(strings.ReplaceAll(*connType, "，", ","), ",") {
		tp = strings.ToLower(strings.TrimSpace(tp))
		if tp == "" {
			continue
		}
		if _, ok := allowed[tp]; !ok {
			return fmt.Errorf("不支持连接类型 %q，精简版仅支持 tcp/tls", tp)
		}
	}
	return nil
}

// configureLogging 根据最小参数集初始化日志系统。
func configureLogging() {
	if strings.EqualFold(*logType, "false") {
		*logType = "off"
	}
	if *debug && *logType != "off" {
		if *logType != "both" {
			*logType = "stdout"
		}
		*logLevel = "trace"
	}
	if *logPath == "" || strings.EqualFold(*logPath, "on") || strings.EqualFold(*logPath, "true") {
		*logPath = common.GetNpcLogPath()
	}
	if !filepath.IsAbs(*logPath) {
		*logPath = filepath.Join(common.GetRunPath(), *logPath)
	}
	if common.IsWindows() {
		*logPath = strings.ReplaceAll(*logPath, "\\", "\\\\")
	}
	logs.Init(*logType, *logLevel, *logPath, *logMaxSize, *logMaxFiles, *logMaxDays, *logCompress, *logColor)
}

// run 是最小入口：解析环境变量回退、拆分多组 server/vkey/type，并进入自动重连循环。
func run(ctx context.Context, cancel context.CancelFunc) {
	env := common.GetEnvMap()
	if *serverAddr == "" {
		*serverAddr = env["NPC_SERVER_ADDR"]
	}
	if *verifyKey == "" {
		*verifyKey = env["NPC_SERVER_VKEY"]
	}
	if *verifyKey == "" || *serverAddr == "" {
		logs.Error("serverAddr 或 verifyKey 不能为空")
		os.Exit(1)
	}

	logs.Info("客户端版本=%s，核心协议=%s", version.VERSION, version.GetVersion(*protoVer))
	*serverAddr = strings.ReplaceAll(*serverAddr, "，", ",")
	*serverAddr = strings.ReplaceAll(*serverAddr, "：", ":")
	*verifyKey = strings.ReplaceAll(*verifyKey, "，", ",")
	*connType = strings.ReplaceAll(*connType, "，", ",")

	serverAddrs := common.HandleArrEmptyVal(strings.Split(*serverAddr, ","))
	verifyKeys := common.HandleArrEmptyVal(strings.Split(*verifyKey, ","))
	connTypes := common.HandleArrEmptyVal(strings.Split(*connType, ","))
	if len(connTypes) == 0 {
		connTypes = append(connTypes, "tcp")
	}
	if len(serverAddrs) == 0 || len(verifyKeys) == 0 || serverAddrs[0] == "" || verifyKeys[0] == "" {
		logs.Error("serverAddr 或 verifyKey 不能为空")
		os.Exit(1)
	}

	maxLength := common.ExtendArrs(&serverAddrs, &verifyKeys, &connTypes)
	for i := 0; i < maxLength; i++ {
		serverAddr := serverAddrs[i]
		verifyKey := verifyKeys[i]
		connType := strings.ToLower(connTypes[i])

		go func(serverAddr, verifyKey, connType string) {
			for {
				logs.Info("启动连接 server=%s type=%s", serverAddr, connType)
				client.NewRPClient(serverAddr, verifyKey, connType, *disconnectTime).Start(ctx)
				if *autoReconnect {
					logs.Info("客户端已关闭，五秒后重连")
					time.Sleep(5 * time.Second)
					continue
				}
				logs.Info("客户端已关闭")
				cancel()
				os.Exit(1)
			}
		}(serverAddr, verifyKey, connType)
	}
}
