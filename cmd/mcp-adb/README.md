# 本地 ADB MCP

独立的本地 MCP 服务端，直接调用当前机器上的 `adb`，并可通过 Web API 查询远端 NPS 服务器的设备和隧道信息。

## 架构

```
本地 MCP (cmd/mcp-adb)
├── ADB 工具 —— 调用本地 adb 二进制
│   ├── ADB设备列表、执行命令、截屏、查看日志
│   ├── 应用列表、安装应用、系统属性、重启设备
│   └── ADB配对、开启ADB调试、关闭ADB调试
│
└── NPS 工具（可选）—— 通过 Web API 查询远端 NPS 服务器
    ├── NPS设备列表（客户端在线状态）
    ├── NPS连通测试（Ping RTT）
    └── 隧道列表（按客户端/类型筛选）
```

- **NPS 服务器**：只负责设备接入、NAT 穿透和隧道管理
- **本地 MCP**：负责本地 `adb` 调用 + 远程 NPS 查询，统一暴露为 MCP 工具

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
  --nps-url http://101.34.243.224:8081 \
  --nps-auth-key your-nps-auth-key \
  --connect-addr 101.34.243.224:15555 \
  --pair-addr 101.34.243.224:15556
```

### 2. HTTP 模式（本地联调）

```bash
./local-adb-mcp \
  --transport http \
  --addr 127.0.0.1:18028 \
  --endpoint /mcp \
  --adb-path /home/DeYouOS/Android/Sdk/platform-tools/adb \
  --nps-url http://101.34.243.224:8081 \
  --nps-auth-key your-nps-auth-key \
  --connect-addr 101.34.243.224:15555 \
  --pair-addr 101.34.243.224:15556
```

如需 HTTP 鉴权（保护 MCP 接口本身），可追加：

```bash
--auth-key your-mcp-bearer-token
```

请求头需要带：

```text
Authorization: Bearer your-mcp-bearer-token
```

## 已提供工具

### ADB 工具（本地 adb）

| 工具名 | 说明 |
|--------|------|
| `ADB设备列表` | 列出本地 adb 已连接的所有设备 |
| `执行命令` | 在指定设备上执行 adb shell 命令 |
| `截屏` | 截取设备屏幕（返回 base64 PNG） |
| `查看日志` | 获取 logcat 日志 |
| `应用列表` | 列出已安装的应用包名 |
| `安装应用` | 安装本地 APK 到设备 |
| `ADB配对` | 返回 adb pair 命令 |
| `开启ADB调试` | 返回 adb pair + connect 命令 |
| `关闭ADB调试` | 返回 adb disconnect 命令 |
| `系统属性` | 读取 Android 系统属性 |
| `重启设备` | 重启设备（支持 recovery/bootloader） |

### NPS 工具（远程查询，需配置 `--nps-url`）

| 工具名 | 说明 |
|--------|------|
| `NPS设备列表` | 查询 NPS 服务器上所有客户端及在线状态 |
| `NPS连通测试` | Ping 指定 NPS 客户端，返回 RTT |
| `隧道列表` | 查询隧道列表，支持按客户端 ID 和类型筛选 |

## 参数说明

### 基本参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--transport` | `stdio` | 传输模式：`stdio` 或 `http` |
| `--addr` | `127.0.0.1:18028` | HTTP 监听地址 |
| `--endpoint` | `/mcp` | HTTP MCP 路径 |
| `--auth-key` | 空 | HTTP Bearer 鉴权密钥（保护 MCP 接口） |
| `--adb-path` | `adb` | 本地 `adb` 可执行文件路径 |

### ADB 连接参数（可选静态配置）

| 参数 | 说明 |
|------|------|
| `--connect-addr` | adb connect 地址（NPS 隧道端口） |
| `--pair-addr` | adb pair 地址（Android 11+ 配对） |

### NPS 参数（可选，配置后启用 NPS 工具）

| 参数 | 说明 |
|------|------|
| `--nps-url` | NPS Web 管理地址（如 `http://101.34.243.224:8081`） |
| `--nps-auth-key` | NPS 配置的 `auth_key`（与 `conf/nps.conf` 中一致） |

> 注意：`--nps-auth-key` 在设置 `--nps-url` 时必填。NPS 认证机制为 `MD5(auth_key + timestamp)`，时间戳有效期 20 秒，请确保本机时间准确。
