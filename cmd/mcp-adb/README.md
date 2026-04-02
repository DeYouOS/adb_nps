# 本地 ADB MCP

独立的本地 MCP 服务端，直接调用当前机器上的 `adb`，并可通过 Web API 查询远端 NPS 服务器的设备和隧道信息。

## 架构

```
本地 MCP (cmd/mcp-adb)
├── ADB 工具（23 个）—— 调用本地 adb 二进制
│   ├── 设备管理：ADB设备列表、重启设备、获取Root
│   ├── 命令执行：执行命令、查看日志
│   ├── 应用管理：应用列表、安装应用、卸载应用、启动Activity、停止应用、清除数据
│   ├── 文件操作：推送文件、拉取文件、文件管理
│   ├── 系统信息：系统属性、系统服务信息、进程列表
│   ├── 输入/截屏：截屏、输入操作
│   ├── 网络：端口转发
│   └── 连接辅助：ADB配对、开启ADB调试、关闭ADB调试
│
└── NPS 工具（3 个，可选）—— 通过 Web API 查询远端 NPS 服务器
    ├── NPS设备列表（客户端在线状态）
    ├── NPS连通测试（Ping RTT）
    └── 隧道列表（按客户端筛选，自动过滤 scrcpy 隧道）
```

- **NPS 服务器**：只负责设备接入、NAT 穿透和隧道管理
- **本地 MCP**：负责本地 `adb` 调用 + 远程 NPS 查询，统一暴露为 MCP 工具
- **所有输出**：结构化 JSON 格式，适配 AI 消费

## 构建

```bash
go build -o local-adb-mcp ./cmd/mcp-adb
```

## 运行方式

### 1. stdio 模式（MCP Host 直接拉起）

```bash
./local-adb-mcp \
  --transport stdio \
  --adb-path /home/DeYouOS/Android/Sdk/platform-tools/adb \
  --nps-url http://101.34.243.224:8080 \
  --nps-auth-key nps2026api
```

### 2. HTTP 模式（本地联调）

```bash
./local-adb-mcp \
  --transport http \
  --addr 127.0.0.1:18028 \
  --endpoint /mcp \
  --adb-path /home/DeYouOS/Android/Sdk/platform-tools/adb \
  --nps-url http://101.34.243.224:8080 \
  --nps-auth-key nps2026api
```

如需 HTTP 鉴权（保护 MCP 接口本身），可追加：

```bash
--auth-key your-mcp-bearer-token
```

请求头需要带：

```text
Authorization: Bearer your-mcp-bearer-token
```

## 已提供工具（26 个）

### ADB 工具（23 个，本地 adb）

| 工具名 | 说明 | 输出格式 |
|--------|------|----------|
| `ADB设备列表` | 列出所有设备（自动连接 NPS 在线远程设备） | 结构化 JSON |
| `执行命令` | 在指定设备上执行 adb shell 命令 | 原始文本 |
| `截屏` | 截取设备屏幕（返回 base64 PNG） | 原始文本 |
| `查看日志` | 获取 logcat 日志 | 原始文本 |
| `应用列表` | 列出已安装的应用包名 | 结构化 JSON |
| `安装应用` | 安装本地 APK 到设备 | 结构化 JSON |
| `卸载应用` | 卸载指定应用 | 结构化 JSON |
| `启动Activity` | 启动指定 Activity 或 Intent | 结构化 JSON |
| `停止应用` | 强制停止指定应用 | 结构化 JSON |
| `清除数据` | 清除指定应用的数据和缓存 | 结构化 JSON |
| `推送文件` | 将本地文件推送到设备 | 结构化 JSON |
| `拉取文件` | 从设备拉取文件到本地 | 结构化 JSON |
| `文件管理` | 列出设备上指定目录的文件 | 结构化 JSON |
| `系统属性` | 读取 Android 系统属性（支持单个或全部） | 结构化 JSON |
| `系统服务信息` | 获取 dumpsys 服务信息 | 结构化 JSON |
| `进程列表` | 列出设备上运行的进程 | 结构化 JSON |
| `输入操作` | 模拟按键、文本输入、滑动、点击等 | 结构化 JSON |
| `端口转发` | 管理 adb 端口转发（list/forward/remove） | 结构化 JSON |
| `重启设备` | 重启设备（支持 recovery/bootloader） | 结构化 JSON |
| `获取Root` | 以 root 权限重启 adb daemon | 结构化 JSON |
| `ADB配对` | 返回 adb pair 命令 | 原始文本 |
| `开启ADB调试` | 返回 adb pair + connect 命令 | 原始文本 |
| `关闭ADB调试` | 返回 adb disconnect 命令 | 原始文本 |

### NPS 工具（3 个，远程查询，需配置 `--nps-url`）

| 工具名 | 说明 | 输出格式 |
|--------|------|----------|
| `NPS设备列表` | 查询 NPS 服务器上所有客户端及在线状态 | 结构化 JSON |
| `NPS连通测试` | Ping 指定 NPS 客户端，返回 RTT | 结构化 JSON |
| `隧道列表` | 查询隧道列表（自动过滤 scrcpy 隧道，含客户端信息） | 结构化 JSON |

## 特性

- **自动连接远程设备**：`ADB设备列表` 调用时自动查询 NPS 在线客户端，通过隧道端口 `adb connect` 后统一列出
- **scrcpy 隧道过滤**：每个 NPS 客户端有两条隧道（低端口=ADB，高端口=scrcpy），隧道列表自动过滤 scrcpy 隧道
- **结构化 JSON 输出**：大部分工具输出解析为 JSON 对象/数组，方便 AI 直接处理
- **NPS 可选**：不配置 `--nps-url` 时 NPS 工具不注册，纯本地 ADB 也可正常使用

## 参数说明

### 基本参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--transport` | `stdio` | 传输模式：`stdio` 或 `http` |
| `--addr` | `127.0.0.1:18028` | HTTP 监听地址 |
| `--endpoint` | `/mcp` | HTTP MCP 路径 |
| `--auth-key` | 空 | HTTP Bearer 鉴权密钥（保护 MCP 接口） |
| `--adb-path` | `adb` | 本地 `adb` 可执行文件路径 |

### NPS 参数（可选，配置后启用 NPS 工具和自动连接）

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--nps-url` | 空 | NPS Web 管理地址（如 `http://101.34.243.224:8080`） |
| `--nps-auth-key` | 空 | NPS 配置的 `auth_key`（与 `conf/nps.conf` 中一致） |
| `--nps-proxy` | 空 | 访问 NPS 时使用的 HTTP 代理（国内服务器通常不需要） |

> 注意：`--nps-auth-key` 在设置 `--nps-url` 时必填。NPS 认证机制为 `MD5(auth_key + timestamp)`，时间戳有效期 20 秒，请确保本机时间准确。
