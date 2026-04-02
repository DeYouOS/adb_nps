# 待实现：Android 调试工具支持

> 状态：待实现 | 创建时间：2026-04-03 | 优先级：中

## 目标

让 AI 通过 MCP 工具完成 Android 应用的 Java (JDWP) 和 NDK (gdb/lldb) 调试，覆盖从发现问题到定位根因的完整链路。

## 核心矛盾

MCP 是 **request-response** 模型，调试器 (jdb/gdb/lldb) 是**交互式持久会话**。
解决思路：利用 jdb/lldb 的**批处理模式**绕过交互限制，把"一组调试命令"打包为一次 MCP 调用。

## 现有可复用工具

| 已有工具 | 调试用途 |
|---------|---------|
| `端口转发` (add/remove/list) | `adb forward tcp:PORT jdwp:<pid>` |
| `进程列表` (ps + filter) | 找目标应用 PID |
| `执行命令` (任意 shell) | 跑 `adb jdwp`、`kill -3`、`am dumpheap` 等 |
| `推送文件` / `拉取文件` | 推 lldb-server、拉 heap dump |
| `清空日志` + `查看日志` | 捕获 crash/ANR 日志 |

## 分层实现计划

### 第一层：一次性诊断工具（简单，半天）

纯 shell 命令，request-response，和现有工具同等复杂度。

#### 1. `调试进程列表`

```bash
# 核心命令
adb jdwp                          # 列出可调试 PID
adb shell ps -A | grep <pid>     # PID → 包名映射
```

- 参数：`serial`
- 返回：JSON 数组 `[{pid, package, debuggable}]`
- 注意：`adb jdwp` 会持续输出，需加超时（2秒够了）或用 `timeout 2 adb jdwp`

#### 2. `等待调试器`

```bash
# 设置应用启动时等待调试器
adb shell am set-debug-app -w <package>

# 清除
adb shell am clear-debug-app
```

- 参数：`serial`, `package`, `action`(set/clear)
- 返回：操作结果 JSON

#### 3. `线程转储`

```bash
# 方法1：kill -3 触发 → 输出到 logcat
adb shell kill -3 <pid>
# 等 1 秒
adb logcat -d -t 200 | grep -A 200 "DALVIK THREADS"

# 方法2：直接用 cmd activity（Android 10+）
adb shell cmd activity thread <package>
```

- 参数：`serial`, `package` 或 `pid`
- 返回：线程栈文本
- 注意：kill -3 需要同 UID 或 root

#### 4. `堆转储`

```bash
adb shell am dumpheap <pid> /data/local/tmp/heap.hprof
# 等转储完成（轮询文件大小稳定）
adb pull /data/local/tmp/heap.hprof <local_path>
adb shell rm /data/local/tmp/heap.hprof
```

- 参数：`serial`, `pid` 或 `package`, `local_path`
- 返回：本地文件路径 + 文件大小
- 注意：转储耗时可能较长（几秒到十几秒），需要足够的超时

### 第二层：批处理调试查询（中等，1天）

利用 jdb/lldb 批处理模式，**一次调用完成整个调试查询**。

#### 5. `Java调试查询`

```bash
# 完整流程（工具内部自动完成）：
# 1. adb forward tcp:$PORT jdwp:$PID
# 2. echo -e "commands..." | jdb -attach localhost:$PORT
# 3. adb forward --remove tcp:$PORT

# jdb 支持的一次性查询命令：
threads          # 列出所有线程
where <tid>      # 指定线程的调用栈
where all        # 所有线程的调用栈
classes          # 已加载的所有类
methods <class>  # 类的所有方法
fields <class>   # 类的所有字段
locals           # 当前帧的局部变量（需要在断点处）
print <expr>     # 求值表达式
```

- 参数：`serial`, `pid`, `commands`(字符串数组)
- 返回：jdb 输出文本
- 实现要点：
  - 自动分配空闲端口（`adb forward tcp:0 jdwp:PID` 返回分配的端口）
  - 命令末尾自动追加 `quit`
  - 管道输入：`echo -e "cmd1\ncmd2\nquit" | jdb -attach localhost:PORT`
  - 超时控制：`context.WithTimeout`
  - 自动清理端口转发

#### 6. `NDK调试查询`

```bash
# 前置条件：lldb-server 已在设备上运行
# lldb-server 位置：$ANDROID_NDK/toolchains/llvm/prebuilt/linux-x86_64/lib/clang/20/lib/linux/<arch>/lldb-server

# 推送（如果不存在）
adb push $NDK/.../<arch>/lldb-server /data/local/tmp/
adb shell chmod 755 /data/local/tmp/lldb-server

# 启动（后台运行）
adb shell /data/local/tmp/lldb-server g :5039 --attach <pid> &

# 端口转发
adb forward tcp:5039 tcp:5039

# lldb 批处理查询
lldb --batch \
  -o "platform select remote-android" \
  -o "platform connect connect://localhost:5039" \
  -o "bt all" \
  -o "thread list" \
  -o "quit"

# 清理
adb forward --remove tcp:5039
adb shell pkill lldb-server
```

- 参数：`serial`, `pid`, `commands`(字符串数组)
- 返回：lldb 输出文本
- 前置依赖：本地需要 `lldb`，设备需要对应架构的 `lldb-server`
- 实现要点：
  - 自动检测设备架构：`adb shell getprop ro.product.cpu.abi`
  - 自动推送 lldb-server（如果不存在）
  - 自动管理 lldb-server 生命周期（启动/清理）
  - NDK 路径从 `$ANDROID_NDK` 或 `$ANDROID_SDK/ndk/` 自动发现

### 第三层：交互式单步调试（困难，2-3天）

需要持久进程管理，后续再考虑。

#### 设计思路

```go
// 会话管理器：维护 jdb/lldb 子进程
type debugSession struct {
    id      string
    cmd     *exec.Cmd
    stdin   io.WriteCloser
    stdout  io.ReadCloser
    serial  string
    pid     int
    kind    string // "jdb" | "lldb"
}

var sessions = map[string]*debugSession{}
```

- `调试会话_创建(serial, pid, type)` → 启动 jdb/lldb，返回 session_id
- `调试会话_执行(session_id, command)` → 向 stdin 写命令，读 stdout 返回
- `调试会话_关闭(session_id)` → kill 进程，清理转发

#### 支持的交互式操作

| jdb 命令 | 用途 |
|----------|------|
| `stop in <class>.<method>` | 设置方法断点 |
| `stop at <class>:<line>` | 设置行断点 |
| `clear <class>.<method>` | 清除断点 |
| `cont` / `resume` | 继续执行 |
| `step` | 单步进入 |
| `next` | 单步跳过 |
| `locals` | 查看局部变量 |
| `print <expr>` | 求值表达式 |
| `dump <obj>` | 打印对象详细信息 |

## AI 调试工作流示例

### 场景1：Java 应用 Crash 分析

```
1. 清空日志(serial)
2. 启动Activity(serial, package)
3. 查看日志(serial, since="10", filter="AndroidRuntime:E *:S")
4. → AI 发现 NullPointerException at com.example.Foo.bar(Foo.java:42)
5. 调试进程列表(serial) → 找到 PID
6. Java调试查询(serial, pid, ["threads", "where all"])
7. → AI 分析线程状态和调用栈
```

### 场景2：内存泄漏排查

```
1. 进程列表(serial, filter="com.example")  → 找到 PID
2. 系统服务信息(serial, service="meminfo com.example.app")
3. → AI 发现内存持续增长
4. 堆转储(serial, pid, "/tmp/heap.hprof")
5. → AI 分析 hprof 文件（需要额外工具支持）
```

### 场景3：NDK Crash 分析

```
1. 查看日志(serial, filter="DEBUG:* *:S")  → 看 tombstone
2. → AI 发现 signal 11 (SIGSEGV) in libfoo.so
3. NDK调试查询(serial, pid, ["bt all", "register read"])
4. → AI 分析 native 栈和寄存器状态
```

## 依赖项

| 工具 | 用途 | 位置 |
|------|------|------|
| `jdb` | Java 调试器 | `$JAVA_HOME/bin/jdb` 或 Android Studio JBR |
| `lldb` | Native 调试器 | LLVM 工具链 / Android Studio |
| `lldb-server` | 设备端调试服务 | `$ANDROID_NDK/toolchains/llvm/.../lldb-server` |
| `adb` | 已有 | `--adb-path` 参数 |

## 新增启动参数

```
--jdb-path    jdb 可执行文件路径（默认自动查找）
--lldb-path   lldb 可执行文件路径（默认自动查找）
--ndk-path    Android NDK 路径（用于查找 lldb-server，默认 $ANDROID_NDK）
```
