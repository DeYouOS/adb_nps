# npc_app — Android 客户端实现文档

> 状态：已实现 | 创建时间：2026-09-18
> 目标：把 NPC 编译成 Android 动态库（c-shared），配一个自研 Android app 调用，方便自定义扩展。

---

## 1. 背景

### 现状问题

| 资产 | 问题 |
|------|------|
| `cmd/npc/sdk.go` | **编译失败**（`client.NewRPClient` 签名不匹配，client 包精简后未同步） |
| `build.android.sh` | 产出 `libnpc.so` 是 **PIE 可执行文件**，只能 `exec` 运行，不能 `System.loadLibrary()` |
| `build.assets.sh:build_sdk()` | c-shared 构建逻辑现成，但**只支持 windows/linux，无 Android** |
| Android app | **不存在** |

### 已验证结论

修复 `sdk.go` 后，用 NDK 可成功编译出**真·动态库**：

```
libnpc_sdk_arm64.so: ELF 64-bit LSB shared object, ARM aarch64
导出: StartClientByVerifyKey / GetClientStatus / CloseClient / Version / SetLogsLevel / Logs / SetDnsServer
```

验证命令（已实测通过）：
```bash
NDK_ROOT="$HOME/Android/Sdk/ndk/27.0.12077973"
LLVM_BIN="$NDK_ROOT/toolchains/llvm/prebuilt/linux-x86_64/bin"
CGO_ENABLED=1 GOOS=android GOARCH=arm64 CC="$LLVM_BIN/aarch64-linux-android24-clang" \
  go build -tags sdk -buildmode=c-shared -o libnpc.so ./cmd/npc/sdk.go
```

---

## 2. 架构设计

```
┌─────────────────────────────────────────────┐
│              npc_app (APK)                  │
│                                             │
│  Kotlin UI (MainActivity)                   │
│      │  external fun                        │
│      ▼                                      │
│  NpcNative.kt  ── JNI ──▶  libnpcjni.so     │  ← C wrapper
│                                 │           │
│                                 ▼           │
│                            libnpc.so        │  ← Go c-shared（sdk.go）
│                                 │           │
│                        ┌────────┴────────┐  │
│                        │  client.TRPClient│  │
│                        │  mux tunnel      │  │
│                        └────────┬────────┘  │
└─────────────────────────────────┼───────────┘
                                  ▼
                          NPS Server (tcp/tls)
```

**为什么需要 JNI wrapper？**
Go 的 `c-shared` 导出的是**普通 C 函数**（`StartClientByVerifyKey`），不是 JNI 函数（`Java_xxx_yyy`）。Android 的 `System.loadLibrary` 只会调用 JNI 符号，所以中间必须有一层 C 桥接。

---

## 3. 目录结构

```
npc_app/                                  ← 新建（Android 工程）
├── settings.gradle
├── build.gradle
├── gradle.properties
├── gradlew / gradlew.bat
└── app/
    ├── build.gradle
    └── src/main/
        ├── AndroidManifest.xml
        ├── java/com/npc/app/
        │   ├── MainActivity.kt          ← UI + 生命周期 + 日志脱敏
        │   ├── NpcNative.kt             ← JNI 方法声明
        │   └── NpcService.kt            ← 前台服务 + NpcMask 日志脱敏
        ├── assets/
        │   └── npc_config.json          ← 注入模板（服务端打包时替换）
        ├── cpp/
        │   ├── CMakeLists.txt
        │   └── npc_jni.c                ← JNI wrapper
        ├── jniLibs/
        │   ├── arm64-v8a/libnpc.so      ← Go c-shared 产物（预编译）
        │   ├── armeabi-v7a/libnpc.so
        │   ├── x86_64/libnpc.so
        │   └── x86/libnpc.so
        └── res/...
pack_apk.sh                              ← 本地离线打包脚本
```

---

## 4. Go 侧改动

### 4.1 修复 `cmd/npc/sdk.go`（必须）

```go
// 原（编译失败）：
cl = client.NewRPClient(serverAddr, verifyKey, connType, proxyUrl, "", "", nil, 60, nil)

// 修复：
cl = client.NewRPClient(serverAddr, verifyKey, connType, 60)
```

### 4.2 改为异步启动（必须）

**问题**：`cl.Start(ctx)` 会阻塞直到客户端退出。JNI 调用会卡住调用线程（Android 主线程卡死 → ANR）。

```go
//export StartClientByVerifyKey
func StartClientByVerifyKey(serverAddr, verifyKey, connType, proxyUrl *C.char) int {
	clMu.Lock()
	if cl != nil {
		cl.Close()
	}
	cl = client.NewRPClient(C.GoString(serverAddr), C.GoString(verifyKey), C.GoString(connType), 60)
	c := cl
	clMu.Unlock()

	go c.Start(context.Background())   // ← 异步，立即返回
	return 1
}
```

### 4.3 加并发保护（建议）

`cl` 是全局变量，多线程调用 `StartClientByVerifyKey` / `CloseClient` 会竞态：

```go
var (
	cl   *client.TRPClient
	clMu sync.Mutex
)
```

`CloseClient`、`GetClientStatus` 同样加锁。

### 4.4 参数扩展（可选，方便自定义）

当前 sdk.go 硬编码 `disconnectTime=60`，且 `proxyUrl` 未使用。可按需扩展：

```go
//export StartClientEx
func StartClientEx(serverAddr, verifyKey, connType, proxyUrl *C.char, disconnectSec C.int) int
```

---

## 5. JNI 桥接层

### 5.1 `app/src/main/cpp/npc_jni.c`

```c
#include <jni.h>
#include <stdlib.h>

/* ---- Go c-shared 导出的 C 接口（对应 libnpc.so） ---- */
extern int   StartClientByVerifyKey(char* serverAddr, char* verifyKey, char* connType, char* proxyUrl);
extern int   GetClientStatus(void);
extern void  CloseClient(void);
extern char* Version(void);
extern char* Logs(void);
extern void  SetLogsLevel(char* level);
extern void  SetDnsServer(char* dns);

/* ---- JNI 方法：类 com.npc.app.NpcNative ---- */

JNIEXPORT jint JNICALL
Java_com_npc_app_NpcNative_startClient(JNIEnv *env, jclass clazz,
                                       jstring jServer, jstring jVkey,
                                       jstring jType, jstring jProxy) {
    const char *s = (*env)->GetStringUTFChars(env, jServer, 0);
    const char *k = (*env)->GetStringUTFChars(env, jVkey, 0);
    const char *t = (*env)->GetStringUTFChars(env, jType, 0);
    const char *p = jProxy ? (*env)->GetStringUTFChars(env, jProxy, 0) : "";

    jint ret = (jint) StartClientByVerifyKey((char*) s, (char*) k, (char*) t, (char*) p);

    (*env)->ReleaseStringUTFChars(env, jServer, s);
    (*env)->ReleaseStringUTFChars(env, jVkey, k);
    (*env)->ReleaseStringUTFChars(env, jType, t);
    if (jProxy) (*env)->ReleaseStringUTFChars(env, jProxy, p);
    return ret;
}

JNIEXPORT jint JNICALL
Java_com_npc_app_NpcNative_getStatus(JNIEnv *env, jclass clazz) {
    return (jint) GetClientStatus();
}

JNIEXPORT void JNICALL
Java_com_npc_app_NpcNative_closeClient(JNIEnv *env, jclass clazz) {
    CloseClient();
}

JNIEXPORT jstring JNICALL
Java_com_npc_app_NpcNative_version(JNIEnv *env, jclass clazz) {
    char *v = Version();
    return (*env)->NewStringUTF(env, v ? v : "unknown");
}

JNIEXPORT jstring JNICALL
Java_com_npc_app_NpcNative_logs(JNIEnv *env, jclass clazz) {
    char *l = Logs();
    return (*env)->NewStringUTF(env, l ? l : "");
}

JNIEXPORT void JNICALL
Java_com_npc_app_NpcNative_setLogsLevel(JNIEnv *env, jclass clazz, jstring jLevel) {
    const char *lv = (*env)->GetStringUTFChars(env, jLevel, 0);
    SetLogsLevel((char*) lv);
    (*env)->ReleaseStringUTFChars(env, jLevel, lv);
}

JNIEXPORT void JNICALL
Java_com_npc_app_NpcNative_setDnsServer(JNIEnv *env, jclass clazz, jstring jDns) {
    const char *d = (*env)->GetStringUTFChars(env, jDns, 0);
    SetDnsServer((char*) d);
    (*env)->ReleaseStringUTFChars(env, jDns, d);
}
```

### 5.2 `app/src/main/cpp/CMakeLists.txt`

```cmake
cmake_minimum_required(VERSION 3.22)
project(npcjni)

# 预编译的 Go 动态库
add_library(npc SHARED IMPORTED)
set_target_properties(npc PROPERTIES IMPORTED_LOCATION
    ${CMAKE_SOURCE_DIR}/../jniLibs/${ANDROID_ABI}/libnpc.so)

# JNI wrapper
add_library(npcjni SHARED npc_jni.c)
target_link_libraries(npcjni npc log)
```

---

## 6. Android app 侧

### 6.1 `NpcNative.kt`

```kotlin
package com.npc.app

object NpcNative {
    init {
        System.loadLibrary("npc")      // Go c-shared
        System.loadLibrary("npcjni")   // JNI wrapper
    }

    external fun startClient(server: String, vkey: String, connType: String, proxy: String?): Int
    external fun getStatus(): Int
    external fun closeClient()
    external fun version(): String
    external fun logs(): String
    external fun setLogsLevel(level: String)
    external fun setDnsServer(dns: String)
}
```

### 6.2 `app/build.gradle`（关键片段）

```groovy
android {
    namespace 'com.npc.app'
    compileSdk 34
    defaultConfig {
        applicationId "com.npc.app"
        minSdk 24
        targetSdk 34
        ndk {
            abiFilters 'arm64-v8a', 'armeabi-v7a', 'x86_64', 'x86'
        }
        externalNativeBuild {
            cmake { cppFlags "" }
        }
    }
    externalNativeBuild {
        cmake {
            path "src/main/cpp/CMakeLists.txt"
            version "3.22.1"
        }
    }
}
```

### 6.3 `MainActivity.kt`（极简版）

功能：server/vkey 输入 → 连接/断开 → 状态轮询 → 日志查看

```kotlin
class MainActivity : AppCompatActivity() {
    private val handler = Handler(Looper.getMainLooper())

    // 状态轮询（1s）
    private val statusPoller = object : Runnable {
        override fun run() {
            val online = NpcNative.getStatus() == 1
            tvStatus.text = if (online) "在线" else "离线"
            handler.postDelayed(this, 1000)
        }
    }

    fun onConnectClick() {
        val server = etServer.text.toString()
        val vkey = etVkey.text.toString()
        // 注意：startClient 内部已异步，但为保险仍在子线程调用
        thread {
            NpcNative.setLogsLevel("info")
            NpcNative.startClient(server, vkey, "tls", "")
        }
    }

    fun onDisconnectClick() {
        thread { NpcNative.closeClient() }
    }

    fun onShowLogsClick() {
        thread {
            val logs = NpcNative.logs()
            runOnUiThread { tvLogs.text = logs }
        }
    }
}
```

### 6.4 保活（可选，推荐）

Android 会杀后台进程。生产使用建议加**前台服务**：

```xml
<uses-permission android:name="android.permission.FOREGROUND_SERVICE" />
<uses-permission android:name="android.permission.POST_NOTIFICATIONS" />
<uses-permission android:name="android.permission.INTERNET" />
```

```kotlin
class NpcService : Service() {
    override fun onStartCommand(...): Int {
        startForeground(1, buildNotification())
        return START_STICKY
    }
}
```

---

## 7. 构建流程

### 7.1 Go 库编译脚本：`build.android.sdk.sh`（新建）

参考 `build.android.sh` 的 NDK 检测逻辑，改为 `-buildmode=c-shared`：

```bash
#!/usr/bin/env bash
set -euo pipefail

APP_NAME="npc"
SRC="./cmd/npc/sdk.go"
TAGS="${TAGS:-sdk,netgo,osusergo}"
API="${API:-24}"
NDK_VERSION="${NDK_VERSION:-r28c}"
OUT_ROOT="npc_app/app/src/main/jniLibs"

# ...（复用 build.android.sh 的 ensure_ndk / host_tag 逻辑）...

build_one() {
  local outdir="$1" goarch="$2" cc="$3" extra_env="${4:-}"
  mkdir -p "$OUT_ROOT/$outdir"
  local bin="$OUT_ROOT/$outdir/lib${APP_NAME}.so"
  echo "==> Building $outdir ($goarch) -> $bin"

  local -a ENVV=(CGO_ENABLED=1 GOOS=android GOARCH="$goarch" CC="$cc")
  [[ -n "$extra_env" ]] && ENVV+=("$extra_env")

  env "${ENVV[@]}" \
    go build -trimpath -buildvcs=false -tags "$TAGS" \
      -buildmode=c-shared \
      -ldflags "-s -w -buildid= -linkmode=external -extldflags '-Wl,-z,max-page-size=16384'" \
      -o "$bin" "$SRC"

  "$STRIP_BIN" -s "$bin" || true
}

main() {
  ensure_ndk
  build_one "arm64-v8a"   "arm64" "$AARCH64_CLANG"
  build_one "x86_64"      "amd64" "$X86_64_CLANG"
  build_one "armeabi-v7a" "arm"   "$ARMV7A_CLANG" "GOARM=7"
  build_one "x86"         "386"   "$I686_CLANG"
  echo "OK: $OUT_ROOT"
}
main "$@"
```

> ⚠️ 注意：`-buildmode=c-shared` 与 `-buildmode=pie` **不能混用**。c-shared 不支持 `-trimpath` 之外的 PIE 参数冲突时需调整。
> ⚠️ 16KB page size 对齐（`-Wl,-z,max-page-size=16384`）是 Android 15+ 的硬性要求，务必保留。

### 7.2 app 构建

```bash
# 1. 先编译 Go 库到 jniLibs
./build.android.sdk.sh

# 2. 再构建 APK
#    注意：Gradle 8.13 不支持 Java 25，需用 Java 21
cd npc_app && JAVA_HOME=/usr/lib/jvm/java-21-openjdk-amd64 ./gradlew assembleDebug
```

> **⚠️ 构建顺序很重要**：`jniLibs/**/*.so` 和 `**/*.h` 已加入 `.gitignore`
> （Go 编译产物，4 架构约 27MB）。克隆仓库后必须先执行 `./build.android.sdk.sh`
> 生成动态库，否则 CMake 链接 `libnpc.so` 会失败。
>
> **环境要求**（P1 实测）：Gradle 8.13 / AGP 8.7.3 / Kotlin 1.9.24 / CMake 3.22.1 / JDK 21。
> 若 `~/.gradle/gradle.properties` 配了代理且代理未启动，需临时注释掉，否则依赖下载失败。

---

## 8. 实施步骤

| 阶段 | 内容 | 产出 |
|------|------|------|
| **P0** | 修复 `sdk.go`（签名 + 异步 + 加锁） | 可编译的 SDK 源码 |
| **P0** | 写 `build.android.sdk.sh`，编译 4 架构 `libnpc.so` | jniLibs/ 下 4 个真动态库 |
| **P1** | 写 JNI wrapper（`npc_jni.c` + `CMakeLists.txt`） | `libnpcjni.so` |
| **P1** | 建 Android 工程骨架（gradle + manifest + NpcNative.kt） | 可编译的 APK |
| **P2** | 写 MainActivity（连接/断开/状态/日志） | 可用 app |
| **P3** | 前台服务保活 + 配置持久化 | 生产可用 |
| **P3** | 一键打包（Web 端 + 服务端签名） | 客户免配置安装 |
| **P3** | 服务器地址隐藏（UI + 日志脱敏） | 服务端信息不暴露 |
| **P4** | （可选）ADB 相关功能扩展 | 自定义功能 |

---

## 9. 风险与注意事项

| 风险 | 说明 | 对策 |
|------|------|------|
| **Go runtime 与 Android 信号冲突** | Go runtime 自带 signal handler，可能与 Android 冲突 | 实测验证；必要时用 `debug.SetPanicOnFault` 等调整 |
| **JNI 线程阻塞** | `Start` 阻塞导致 ANR | Go 侧 `go c.Start()` 异步（见 4.2） |
| **全局变量竞态** | `cl` 无锁访问 | 加 `sync.Mutex`（见 4.3） |
| **`System.loadLibrary` 顺序** | `libnpcjni.so` 依赖 `libnpc.so` | 先 load `npc` 再 load `npcjni` |
| **NDK 版本** | 不同 NDK 的 clang 路径不同 | 脚本里做多版本探测（复用 `build.android.sh` 逻辑） |
| **ABI 覆盖** | 只编 arm64 会导致 32 位设备装不上 | `abiFilters` 与实际编译架构保持一致 |
| **Android 15+ 16KB 页** | 未对齐会启动失败 | `-Wl,-z,max-page-size=16384` |
| **日志缓冲上限** | `logs.EnableInMemoryBuffer(0)` = 64KB，会滚动覆盖 | 需要更长日志就调大或落盘 |

---

## 10. 待确认

- [ ] app 包名（暂定 `com.npc.app`）
- [ ] minSdk（暂定 24）
- [ ] 是否需要前台服务保活（生产建议要）
- [ ] 是否需要 ADB 功能（本项目特有：adbctl / ADB 隧道管理）
- [ ] 是否需要多服务器配置管理

---

## 附：已验证的关键命令

```bash
# 修复 sdk.go 后，验证 c-shared 编译（已实测通过）
NDK_ROOT="$HOME/Android/Sdk/ndk/27.0.12077973"
LLVM_BIN="$NDK_ROOT/toolchains/llvm/prebuilt/linux-x86_64/bin"
CGO_ENABLED=1 GOOS=android GOARCH=arm64 \
  CC="$LLVM_BIN/aarch64-linux-android24-clang" \
  go build -tags sdk -buildmode=c-shared -o libnpc.so ./cmd/npc/sdk.go

file libnpc.so
# => ELF 64-bit LSB shared object, ARM aarch64   ← 真动态库 ✅
```

---

## 11. 一键打包（Web 端下载 APK）

### 11.1 概述

NPS Web 管理界面新增「打包 APK」按钮，管理员在客户端列表中点击即可为指定客户端生成预配置的 APK 并下载。APK 内已注入该客户端的 `server`、`vkey`、`conn_type` 等连接参数，客户拿到后安装即可自动连接，无需手动输入。

### 11.2 架构与数据流

```
Web UI（客户端列表页）
    │
    │  点击「打包 APK」按钮
    │  packApk(id)  →  GET /client/pack?id=<clientId>（需登录会话）
    │
    ▼
服务端 ClientController.Pack()
    │
    ├─ 1. 读取 nps.conf 中的 pack_* 配置
    ├─ 2. 查缓存：sha256(vkey|server|conn_type|模板size-mtime)[:16]
    │       命中 → 直接返回缓存文件
    │       未命中 ↓
    ├─ 3. 构造 npc_config.json
    │       {"server":"<服务端地址>","conn_type":"tcp","vkey":"<vkey>","client_id":17,"remark":"..."}
    ├─ 4. rewriteApkConfig()：打开模板 APK，替换 assets/npc_config.json
    ├─ 5. zipalign -p -f 4
    ├─ 6. java -jar apksigner.jar sign
    ├─ 7. 移入缓存目录
    └─ 8. 以 application/vnd.android.package-archive 返回
```

### 11.3 缓存机制

| 项目 | 说明 |
|------|------|
| **缓存 key** | `sha256(vkey|serverAddr|connType|模板size-模板mtime)[:16]` |
| **文件名** | `npc_<clientId>_<hash>.apk`，例如 `npc_17_a1b2c3d4e5f6g7h8.apk` |
| **命中** | 同一客户端、同一服务端地址、同一连接类型、模板未变时，直接返回缓存文件，无需重新签名 |
| **失效** | 模板 APK 的文件大小或修改时间变化时，缓存 key 自动改变，旧缓存自然失效。**更新模板后无需手动清理缓存目录** |

### 11.4 服务器部署清单

以 `/etc/nps/` 为根目录，需准备以下文件和配置：

| 路径 | 说明 |
|------|------|
| `/etc/nps/npc_template.apk` | 模板 APK（release 构建产物，内含空的 `assets/npc_config.json`） |
| `/etc/nps/npc-release.keystore` | 签名密钥库 |
| `/etc/nps/tools/apksigner.jar` | Android SDK 中的 apksigner |
| `/etc/nps/tools/zipalign` | Android SDK 中的 zipalign（**依赖系统 `libc++`**，缺失时报 `error while loading shared libraries: libc++.so`） |
| `/etc/nps/packed/` | 缓存目录（自动创建，权限 0755） |
| `/etc/nps/nps.conf` | `# APK Packing Settings` 区块（见下） |

**`nps.conf` 中的 `pack_*` 配置项：**

```conf
# APK Packing Settings
pack_enabled=true
pack_template=/etc/nps/npc_template.apk
pack_keystore=/etc/nps/npc-release.keystore
pack_keystore_pass=<keystore-password>
pack_keystore_alias=npc
pack_cache_dir=/etc/nps/packed
pack_apksigner_jar=/etc/nps/tools/apksigner.jar
pack_zipalign=/etc/nps/tools/zipalign
pack_server_addr=<服务端地址>
pack_conn_type=tcp
```

部署后执行：

```bash
sudo systemctl restart nps.service
```

### 11.5 更新模板 APK

当 app 代码变更需要重新打包时：

```bash
# 1. 在开发机上构建 release APK
cd npc_app && ./gradlew assembleRelease

# 2. 将产物上传到服务器，覆盖模板
scp app/build/outputs/apk/release/app-release.apk root@<服务器>:/etc/nps/npc_template.apk

# 3. 无需手动清理缓存——模板的 size/mtime 变化后旧缓存自动失效
```

### 11.6 本地离线打包脚本

`pack_apk.sh` 提供了不依赖 NPS Web 服务的本地打包方式，适用于批量预打包或离线环境。

```bash
# 命令行参数
./pack_apk.sh --id 17 --server <服务端地址> --vkey <vkey> --remark "设备名"

# 交互式（省略参数会提示输入）
./pack_apk.sh
```

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `--id` | 客户端 ID（必填） | — |
| `--vkey` | 客户端 VerifyKey（必填） | — |
| `--server` | 服务端地址 | `<服务端地址>` |
| `--conn-type` | 连接类型 | `tcp` |
| `--template` | 模板 APK 路径 | `npc_app/app/build/outputs/apk/release/app-release.apk` |
| `--keystore` | 签名密钥库 | `npc_app/npc-release.keystore` |
| `--output` | 输出路径 | `npc_<id>_packed.apk` |

依赖：`JAVA_HOME`（java 命令）、Android SDK build-tools（zipalign + apksigner.jar）、Python 3（用于重写 zip）。

---

## 12. 服务器地址隐藏

### 12.1 目标

客户安装 APK 后，在 app 界面上**看不到服务端地址**，降低服务端暴露风险。

### 12.2 实现方式

从两个维度隐藏：

**UI 配置区（displayConfig）：**

`MainActivity.kt` 的 `displayConfig()` 方法只展示以下字段：

```kotlin
tvConfig.text = buildString {
    append("Remark: ${cfg.remark.ifEmpty { "-" }}\n")
    append("Client ID: ${cfg.clientId}\n")
    append("Type: ${cfg.connType}")
}
```

**不显示 Server 行。** 客户打开 app 看不到服务端 IP 和端口。

**日志脱敏（NpcMask）：**

`NpcService.kt` 中定义了 `NpcMask` 对象：

```kotlin
object NpcMask {
    fun hide(text: String, server: String?): String {
        if (server.isNullOrEmpty()) return text
        var out = text
        out = out.replace(server, "***")       // 替换 ip:port
        val host = server.substringBefore(":")
        if (host.isNotEmpty()) out = out.replace(host, "***")  // 替换裸 IP
        return out
    }
}
```

调用点：

| 位置 | 代码 | 说明 |
|------|------|------|
| `MainActivity.kt` logPoller | `NpcMask.hide(newLogs, config?.server)` | 每 2s 轮询日志，累积前脱敏 |
| `MainActivity.kt` logPoller | `NpcMask.hide(logAccumulator.toString(), config?.server)` | 展示到 UI 前再次脱敏 |
| `NpcService.kt` statusPoller | `NpcMask.hide(newLogs.take(200), serverAddr)` | logcat 输出前脱敏 |

效果：日志行由 `已连接服务端 <ip>:<port>` 变为 `已连接服务端 ***`。

### 12.3 已知限制

> ⚠️ APK 内的 `assets/npc_config.json` 仍然是**明文 JSON**。客户如果用 `unzip -p npc.apk assets/npc_config.json` 解包，可以看到完整的 `server`、`vkey` 等字段。
>
> 如果需要真正隐藏服务端地址，需要额外实现**配置加密/下发机制**（例如服务端动态下发加密配置、首次连接后擦除本地配置等），当前未实现。

---

## 13. 已验证

以下为实际部署后验证通过的结果：

- 服务端 `pack` 接口在已登录会话下返回 HTTP 200、`Content-Type: application/vnd.android.package-archive`
- 生成的 APK：`v1=false / v2=true / v3=true` 签名校验通过（apksigner verify）
- 注入结果示例（`unzip -p` 查看）：

  ```json
  {"client_id":17,"conn_type":"tcp","remark":"windos远程adb","server":"***","vkey":"<vkey>"}
  ```

- 设备实测（小米 Mi 10 / Android 13）：安装后免输入自动连接，服务端客户端状态 `online=true`
- 日志脱敏生效：日志行由 `已连接服务端 <ip>:<port>` 变为 `已连接服务端 ***`
- UI 配置区不再出现 `Server:` 行
- 缓存 key 随模板指纹变化：模板更新后缓存文件名中的 hash 部分改变（示例 `npc_17_<hash>.apk`）

---

## 14. 权限引导（客户自助授权）

### 14.1 为什么需要

非 root 手机上，Android 系统会主动杀后台进程。NPC 隧道要长期在线，需要用户手动授予以下权限：

- **悬浮窗**：没有则连接状态浮标无法显示
- **通知**：没有则前台服务无法正常运行（Android 13+ 必须运行时授权）
- **电池优化白名单**：没有则系统会在几分钟后杀掉后台进程
- **自启动**：没有则手机重启后隧道不会自动恢复

不给授权的后果：隧道被系统杀掉、悬浮窗不显示、重启后不自动连接。代码层面没有任何 root/系统签名绕过手段，只能走引导流程。

### 14.2 四项权限

| # | 权限 | 必要性 | 可检测 | 检测方式 | 不可检测项处理 |
|---|------|--------|--------|----------|----------------|
| 1 | 悬浮窗（显示在其他应用上层） | 必须 | ✅ | `Settings.canDrawOverlays(ctx)` | — |
| 2 | 通知 | 必须 | ✅ | A13+: `checkSelfPermission(POST_NOTIFICATIONS)` **且** `NotificationManagerCompat.areNotificationsEnabled()`；A13 以下只查后者 | — |
| 3 | 电池优化白名单 | 必须 | ✅ | `PowerManager.isIgnoringBatteryOptimizations(pkg)` | — |
| 4 | 自启动 / 后台运行 | 必须 | ❌ | 无可靠 API | 用户点「我已设置好」→ 写入 `npc_prefs` 的 `perm_autostart_confirmed` |

AOSP 原生 ROM 无自启动概念，该项自动视为已满足（`detectable = false`，引导页直接显示「✅ 无需设置」）。

### 14.3 门控规则

```
MainActivity.onCreate()
  ├─ config 有效？
  │   ├─ 否 → 显示 "No valid config"
  │   └─ 是 → PermissionUtils.allSatisfied()
  │       ├─ 全绿（4/4）→ startForegroundService()
  │       └─ 有未满足项 → 启动 PermissionGuideActivity
  │
PermissionGuideActivity
  ├─ 显示 4 张权限卡片（ROM 专属文案 + 跳转按钮）
  ├─ 每张卡片实时检测：✅ 已开启 / ❌ 未开启
  ├─ 底部按钮：「还差 N 项设置」（灰，不可点）
  │   或「全部就绪，启动服务」（绿，可点）
  └─ onResume 自动刷新 + 电池项延迟 1200ms 二次刷新
```

**判定逻辑**：可检测项（1/2/3）必须真绿 + 不可检测项（4）必须用户确认 → 才允许启动 `NpcService`。否则停在引导页。

### 14.4 ROM 识别

`RomDetector.kt` 通过 `Build.MANUFACTURER` / `Build.BRAND` + `getprop` 系统属性判断 ROM 家族。识别顺序有讲究：HyperOS 必须在 MIUI 之前判断（HyperOS 设备上 `ro.miui.ui.version.name` 可能仍非空）。

| ROM 家族 | 厂商标识 | 判定属性 | 显示名 |
|----------|----------|----------|--------|
| HyperOS | `xiaomi` / `redmi` / `poco` | `ro.mi.os.version.name` 非空 | 小米 HyperOS |
| MIUI | 同上 | `ro.miui.ui.version.name` 非空（HyperOS 未命中时） | 小米 MIUI |
| ColorOS | `oppo` / `realme` / `oneplus` | `ro.build.version.oplusrom`（新）或 `ro.build.version.opporom`（旧）非空 | OPPO ColorOS |
| OriginOS | `vivo` / `iqoo` | `ro.vivo.os.version` 含 `Origin`，或 `ro.build.display.id` 含 `Origin` | vivo OriginOS |
| Funtouch | `vivo` / `iqoo` | `ro.vivo.os.version` 非空但不含 `Origin`；Android 11+ 无属性时兜底 OriginOS | vivo Funtouch |
| EMUI | `huawei` | `ro.build.version.emui` 非空 | 华为 EMUI |
| HarmonyOS | `huawei` | `hw_sc.build.platform.version` 非空 | 华为 HarmonyOS |
| MagicOS | `honor` / `huawei`（早期） | `ro.honor.build.display.id` 或 `ro.build.version.magic` 非空 | 荣耀 MagicOS |
| OneUI | `samsung` | `ro.build.version.oneui` 非空（无此属性也视为 OneUI） | 三星 One UI |
| AOSP | 以上均不匹配 | — | Android（原生） |

> OnePlus（OxygenOS 13+）底层已是 ColorOS，按 ColorOS 组件走。
> 荣耀 Android 12+ 独立后 `brand = honor`；早期仍为 `huawei`。

### 14.5 跳转策略

每项权限的跳转遵循统一模式（`PermissionUtils.launchFirstResolvable()`）：

```
ROM 专属候选 Intent（按顺序）
    ↓ 失败
AOSP 通用 Intent（如 ACTION_MANAGE_OVERLAY_PERMISSION）
    ↓ 失败
ACTION_APPLICATION_DETAILS_SETTINGS（最终兜底）
```

关键实现细节：

- 每个 Intent 先 `resolveActivity()` 判定，再 `startActivity()`
- `ActivityNotFoundException`（组件不存在）和 `SecurityException`（OPPO/vivo 自启动常见）都 catch，继续尝试下一个候选
- 最终兜底 `ACTION_APPLICATION_DETAILS_SETTINGS` 也包裹在 try/catch 中
- **引导必须在 Activity 中发起**：Android 10+ 禁止从 Service 后台启动设置页，所有跳转只在 `PermissionGuideActivity`（用户可见 Activity）中执行
- Android 13+ 通知权限：优先弹出系统运行时权限对话框（`requestPermissions(POST_NOTIFICATIONS)`），用户拒绝后再引导去设置页

### 14.6 实测结论（Xiaomi 11 / HyperOS V816 / Android 14）

在 Xiaomi 11（`M2011K2C`）实测发现了一个关键问题，**推翻了** XXPermissions 文档中「HyperOS 不要走 MIUI 权限编辑器」的建议：

**悬浮窗跳转**：
- 走 AOSP 的 `ACTION_MANAGE_OVERLAY_PERMISSION` → 落到「显示在其他应用的上层」**应用列表页**，客户要在长长的列表里找到自己的 app，体验很差
- 改用 MIUI 的 `PermissionsEditorActivity`（`extra_pkgname`）→ 落到**本应用权限页**，再点「其他权限」即可看到「显示悬浮窗」开关，路径清晰

**代码已按实测调整**：`PermissionGuides.kt` 中 MIUI/HyperOS 的悬浮窗候选 Intent 第一项就是 `com.miui.permcenter.permissions.PermissionsEditorActivity`。

**电池优化跳转后的延迟重检**：
- MIUI/HyperOS（Android 14）用户在电池页面操作后返回引导页，`isIgnoringBatteryOptimizations()` 的状态变化需要约 1000ms 才能读到
- 代码在 `onResume` 中做了延迟 1200ms 的二次刷新（`BATTERY_RECHECK_DELAY_MS = 1200L`），避免用户授权后仍显示「未开启」

### 14.7 已验证

以下为 Xiaomi 11（HyperOS V816 / Android 14 / `M2011K2C`）上完整走查的结果：

- 全新安装并清空授权后，启动落在 `PermissionGuideActivity`，`NpcService` 未启动（门控生效）
- ROM 识别正确：`小米 HyperOS · Android 14 (API 34) · Xiaomi M2011K2C`
- 悬浮窗「去设置」落到 `com.miui.permcenter.permissions.PermissionsEditorActivity`（标题为本应用名）
- 通知落到 `Settings$AppNotificationSettingsActivity`；电池落到 `com.miui.powercenter.legacypowerrank.PowerDetailActivity`；自启动落到 `AutoStartManagementActivity`
- 授予三项可检测权限后，引导页自动显示 3 项 ✅、底部「还差 1 项设置」，服务仍未启动
- 点自启动「我已设置好」后 4/4 全绿，按钮变「全部就绪，启动服务」；点击后服务以**前台服务**启动，日志显示 `已连接服务端 ***`（地址已脱敏），悬浮窗出现

### 14.8 已知限制

- **自启动跳转不精确**：HyperOS 上 `AutoStartManagementActivity` 落到的是「自启动管理」**列表页**（非应用维度），客户还需在列表中找到本应用手动开启
- **MIUI/HyperOS AppOps 悬浮窗授权可能被系统重置**：`appops set SYSTEM_ALERT_WINDOW allow` 不可靠；`Settings.canDrawOverlays()` 是唯一可信判定
- **华为/荣耀/三星的 `isIgnoringBatteryOptimizations()` 只反映 AOSP Doze**，不反映厂商自己的杀后台机制（华为「启动管理」、三星 Sleeping apps）；自启动项在这些 ROM 上要额外说明「启动管理里也要允许」
- **OPPO/vivo 自启动组件可能抛 `SecurityException`**（系统签名权限），代码已 catch 并继续尝试下一个候选
- **三星电池**：`ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS` 对 sideload 应用可能无效，主要靠用户去「后台使用限制」手动加白
- **BootReceiver 开机恢复的前置条件**：重启后能否自动恢复取决于权限是否仍然齐备；若权限被系统重置（如 MIUI/HyperOS AppOps 悬浮窗），`allSatisfied()` 返回 false，BootReceiver 会静默跳过

### 14.9 开机自动恢复（BootReceiver）

#### 为什么必须做

引导里让用户开了「自启动」权限，但如果 App 自己不监听开机广播，重启后并不会被拉起——「自启动」这项引导就变成了空头支票。`BootReceiver` 的作用是在设备重启后自动恢复隧道服务。

#### 实现

`BootReceiver` 继承 `BroadcastReceiver`，监听以下广播：

| 广播 Action | 说明 |
|-------------|------|
| `Intent.ACTION_BOOT_COMPLETED` | 标准 Android 开机广播 |
| `android.intent.action.QUICKBOOT_POWERON` | 部分厂商（HTC 等）的快速启动广播 |
| `com.htc.intent.action.QUICKBOOT_POWERON` | HTC 特定的快速启动广播 |

不在上述列表中的 Action 会被直接 `return`，不做任何处理。

#### 启动条件（必须同时满足）

`onReceive` 内依次检查两项，任一不满足则静默返回：

1. **配置有效**：从 `assets/npc_config.json` 读取配置（与 `NpcService` / `MainActivity` 同一路径），`server` 和 `vkey` 均非空
2. **权限齐备**：`PermissionUtils.allSatisfied(context, prefs)` 返回 `true`（沿用同一份 `npc_prefs` SharedPreferences）

```kotlin
// 伪代码
val cfg = readConfig(context)
if (cfg == null || cfg.server.isEmpty() || cfg.vkey.isEmpty()) return
val prefs = context.getSharedPreferences("npc_prefs", MODE_PRIVATE)
if (!PermissionUtils.allSatisfied(context, prefs)) return
// → 启动服务
```

#### 启动方式

以**前台服务**启动 `NpcService`：

- Android 8.0+（API 26）：`context.startForegroundService(intent)`
- 低版本：`context.startService(intent)`

`ACTION_BOOT_COMPLETED` 是 Android 12+ 官方明确列出的后台启动前台服务豁免项（[文档](https://developer.android.com/about/versions/12/foreground-services#exemptions)），无需额外 workaround。

#### 安全与性能

- `onReceive` 全包 `try/catch`，异常时只打日志，不崩溃
- 只做本地 I/O（读 assets + SharedPreferences），**不联网**，方法执行完立即返回
- 不持有 WakeLock，不启动额外线程

#### Manifest 声明

```xml
<uses-permission android:name="android.permission.RECEIVE_BOOT_COMPLETED" />

<receiver
    android:name=".BootReceiver"
    android:exported="true">
    <intent-filter>
        <action android:name="android.intent.action.BOOT_COMPLETED" />
        <action android:name="android.intent.action.QUICKBOOT_POWERON" />
        <action android:name="com.htc.intent.action.QUICKBOOT_POWERON" />
    </intent-filter>
</receiver>
```

#### 已验证

- **负向（权限不全）**：权限不全时，`QUICKBOOT_POWERON` 广播触发 `BootReceiver`，日志为 `Not all permissions satisfied — skipping auto-start`，`NpcService` 未启动
- **正向（权限齐备）**：权限齐备时，`force-stop` 后发送同一广播，日志为 `NpcService start requested`，`NpcService` 以 `isForeground=true` 启动，并输出 `已连接服务端 ***`（地址已脱敏）
- **说明**：`BOOT_COMPLETED` 是受保护广播，非 root 设备无法用 `adb shell am broadcast` 直接模拟，因此实测用同一 `<intent-filter>` 内的 `QUICKBOOT_POWERON` 走同一条代码路径
