#!/usr/bin/env bash
# pack_apk.sh - 本地一键打包 NPC APK（注入配置 + zipalign + 签名）
# 用法:
#   ./pack_apk.sh --id 17 --server 101.34.243.224:8024 --vkey xxx --remark "设备名"
#   ./pack_apk.sh   # 交互式输入参数
#
# 依赖:
#   - JAVA_HOME 已设置（需要 java 命令）
#   - Android SDK build-tools 中的 zipalign 和 apksigner.jar
#   - 模板 APK: npc_app/app/build/outputs/apk/release/app-release.apk
#   - 签名密钥: npc_app/npc-release.keystore

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 默认值
TEMPLATE="${SCRIPT_DIR}/npc_app/app/build/outputs/apk/release/app-release.apk"
KEYSTORE="${SCRIPT_DIR}/npc_app/npc-release.keystore"
KEYSTORE_PASS="npcapp123"
KEY_ALIAS="npc"
CONN_TYPE="tcp"
SERVER_ADDR="101.34.243.224:8024"

# 尝试自动检测 build-tools
if [ -z "${ZIPALIGN:-}" ]; then
    for d in "$HOME/Android/Sdk/build-tools"/*/; do
        if [ -x "${d}zipalign" ]; then
            ZIPALIGN="${d}zipalign"
            break
        fi
    done
fi
if [ -z "${APKSIGNER_JAR:-}" ]; then
    for d in "$HOME/Android/Sdk/build-tools"/*/; do
        if [ -f "${d}lib/apksigner.jar" ]; then
            APKSIGNER_JAR="${d}lib/apksigner.jar"
            break
        fi
    done
fi

# 解析命令行参数
CLIENT_ID=""
VKEY=""
REMARK=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --id)       CLIENT_ID="$2"; shift 2 ;;
        --server)   SERVER_ADDR="$2"; shift 2 ;;
        --vkey)     VKEY="$2"; shift 2 ;;
        --remark)   REMARK="$2"; shift 2 ;;
        --conn-type) CONN_TYPE="$2"; shift 2 ;;
        --template) TEMPLATE="$2"; shift 2 ;;
        --keystore) KEYSTORE="$2"; shift 2 ;;
        --ks-pass)  KEYSTORE_PASS="$2"; shift 2 ;;
        --key-alias) KEY_ALIAS="$2"; shift 2 ;;
        --zipalign) ZIPALIGN="$2"; shift 2 ;;
        --apksigner) APKSIGNER_JAR="$2"; shift 2 ;;
        --output)   OUTPUT="$2"; shift 2 ;;
        -h|--help)
            echo "用法: $0 --id <clientId> --vkey <vkey> [--server <addr>] [--remark <name>]"
            echo ""
            echo "参数:"
            echo "  --id         客户端 ID (必填)"
            echo "  --vkey       客户端 VerifyKey (必填)"
            echo "  --server     服务端地址 (默认: ${SERVER_ADDR})"
            echo "  --remark     设备备注名"
            echo "  --conn-type  连接类型 (默认: ${CONN_TYPE})"
            echo "  --template   模板 APK 路径"
            echo "  --keystore   签名密钥库路径"
            echo "  --ks-pass    密钥库密码"
            echo "  --key-alias  密钥别名"
            echo "  --zipalign   zipalign 路径"
            echo "  --apksigner  apksigner.jar 路径"
            echo "  --output     输出 APK 路径"
            exit 0
            ;;
        *) echo "未知参数: $1"; exit 1 ;;
    esac
done

# 交互式输入
if [ -z "$CLIENT_ID" ]; then
    read -rp "客户端 ID: " CLIENT_ID
fi
if [ -z "$VKEY" ]; then
    read -rp "VerifyKey: " VKEY
fi
if [ -z "$REMARK" ]; then
    read -rp "备注名 (可选): " REMARK
fi

# 校验必填参数
if [ -z "$CLIENT_ID" ] || [ -z "$VKEY" ]; then
    echo "错误: --id 和 --vkey 为必填参数" >&2
    exit 1
fi

# 校验文件存在
if [ ! -f "$TEMPLATE" ]; then
    echo "错误: 模板 APK 不存在: $TEMPLATE" >&2
    exit 1
fi
if [ ! -f "$KEYSTORE" ]; then
    echo "错误: 签名密钥库不存在: $KEYSTORE" >&2
    exit 1
fi
if [ -z "${ZIPALIGN:-}" ] || [ ! -x "$ZIPALIGN" ]; then
    echo "错误: zipalign 未找到，请设置 ZIPALIGN 环境变量或确保 Android SDK build-tools 已安装" >&2
    exit 1
fi
if [ -z "${APKSIGNER_JAR:-}" ] || [ ! -f "$APKSIGNER_JAR" ]; then
    echo "错误: apksigner.jar 未找到，请设置 APKSIGNER_JAR 环境变量" >&2
    exit 1
fi

# 创建临时目录
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "==> 客户端 ID: ${CLIENT_ID}"
echo "==> 服务端: ${SERVER_ADDR}"
echo "==> VKey: ${VKEY}"
echo "==> 备注: ${REMARK:-<无>}"
echo ""

# 步骤 1: 注入 npc_config.json
echo "[1/3] 注入配置到 APK..."
CONFIG_JSON="{\"server\":\"${SERVER_ADDR}\",\"conn_type\":\"${CONN_TYPE}\",\"vkey\":\"${VKEY}\",\"client_id\":${CLIENT_ID},\"remark\":\"${REMARK}\"}"

REWRTTEN_APK="${TMPDIR}/rewritten.apk"
cp "$TEMPLATE" "$REWRTTEN_APK"

# 使用 Python 重写 zip（比 shell 更可靠）
python3 -c "
import zipfile, sys, json, shutil, os

src = '${REWRTTEN_APK}'
dst = '${TMPDIR}/injected.apk'
config = '${CONFIG_JSON}'

with zipfile.ZipFile(src, 'r') as zin, zipfile.ZipFile(dst, 'w') as zout:
    for item in zin.infolist():
        if item.filename == 'assets/npc_config.json':
            continue
        data = zin.read(item.filename)
        zout.writestr(item, data)
    zout.writestr('assets/npc_config.json', config)
"

echo "[2/3] zipalign 对齐..."
ALIGNED_APK="${TMPDIR}/aligned.apk"
"$ZIPALIGN" -p -f 4 "${TMPDIR}/injected.apk" "$ALIGNED_APK"

echo "[3/3] apksigner 签名..."
if [ -z "${OUTPUT:-}" ]; then
    OUTPUT="${SCRIPT_DIR}/npc_${CLIENT_ID}_packed.apk"
fi
java -jar "$APKSIGNER_JAR" sign \
    --ks "$KEYSTORE" \
    --ks-pass "pass:${KEYSTORE_PASS}" \
    --ks-key-alias "$KEY_ALIAS" \
    --key-pass "pass:${KEYSTORE_PASS}" \
    --out "$OUTPUT" \
    "$ALIGNED_APK"

echo ""
echo "==> 打包完成: $OUTPUT"
echo "==> 文件大小: $(du -h "$OUTPUT" | cut -f1)"
echo ""
echo "验证:"
echo "  unzip -p \"$OUTPUT\" assets/npc_config.json"
echo "  JAVA_HOME=${JAVA_HOME:-/usr/lib/jvm/java-21-openjdk-amd64} apksigner verify \"$OUTPUT\""
