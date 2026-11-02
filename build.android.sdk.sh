#!/usr/bin/env bash
set -euo pipefail

# ===== Config =====
APP_NAME="npc"
SRC="./cmd/npc/sdk.go"
TAGS="${TAGS:-sdk,netgo,osusergo}"
API="${API:-24}"
NDK_VERSION="${NDK_VERSION:-r28c}"
NDK_CACHE_DIR="${NDK_CACHE_DIR:-$HOME}"
OUT_ROOT="npc_app/app/src/main/jniLibs"
# ===================

host_tag() { echo "$(uname -s | tr '[:upper:]' '[:lower:]')-x86_64"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "need $1"; exit 1; }; }

is_ndk_root() {
  local d="$1" ht; ht="$(host_tag)"
  [[ -d "$d/toolchains/llvm/prebuilt/${ht}/bin" ]]
}

download_ndk_to() {
  local target="$1"
  local parent; parent="$(dirname "$target")"
  local expected="${parent}/android-ndk-${NDK_VERSION}"
  mkdir -p "${parent}"
  local url="https://dl.google.com/android/repository/android-ndk-${NDK_VERSION}-linux.zip"
  echo "Downloading NDK ${NDK_VERSION} -> ${parent} ..."
  need curl; need unzip
  local tmpzip; tmpzip="$(mktemp -t ndk.XXXXXX.zip)"
  curl -fsSL --retry 5 --retry-delay 2 "${url}" -o "${tmpzip}"
  (cd "${parent}" && unzip -q "${tmpzip}") || true
  rm -f "${tmpzip}"
  if [[ "${expected}" != "${target}" ]]; then
    [[ -d "${expected}" ]] || { echo "Downloaded NDK not found at ${expected}"; exit 1; }
    rm -rf "${target}"
    mv "${expected}" "${target}"
  fi
  [[ -d "${target}" ]] || { echo "NDK unzip failed: ${target}"; exit 1; }
  NDK_ROOT="${target}"
}

print_ndk_info() {
  local sp="${NDK_ROOT}/source.properties"
  local rev=""
  if [[ -f "${sp}" ]]; then
    rev="$(grep -E '^Pkg\.Revision\s*=' "${sp}" | sed -E 's/.*=\s*//')"
  fi
  echo "Using NDK_ROOT: ${NDK_ROOT}"
  if [[ -n "${rev}" ]]; then
    echo "NDK Pkg.Revision: ${rev}"
  else
    echo "NDK dir name: $(basename "${NDK_ROOT}")"
  fi
}

ensure_ndk() {
  # 1. Explicit $NDK or $ANDROID_NDK_HOME
  if [[ -n "${NDK:-}" ]]; then
    if [[ -d "${NDK}" ]] && is_ndk_root "${NDK}"; then
      NDK_ROOT="${NDK}"
    else
      echo "\$NDK points to a missing/invalid dir: ${NDK} -> will download"
      download_ndk_to "${NDK}"
    fi
  elif [[ -n "${ANDROID_NDK_HOME:-}" ]]; then
    if [[ -d "${ANDROID_NDK_HOME}" ]] && is_ndk_root "${ANDROID_NDK_HOME}"; then
      NDK_ROOT="${ANDROID_NDK_HOME}"
    else
      echo "\$ANDROID_NDK_HOME points to a missing/invalid dir: ${ANDROID_NDK_HOME} -> will download"
      download_ndk_to "${ANDROID_NDK_HOME}"
    fi
  else
    # 2. Check $HOME/Android/Sdk/ndk/* (Android Studio default)
    local sdk_ndk_dir="${HOME}/Android/Sdk/ndk"
    local found=""
    if [[ -d "${sdk_ndk_dir}" ]]; then
      # Pick the highest version available
      local best=""
      for d in "${sdk_ndk_dir}"/*/; do
        [[ -d "$d" ]] || continue
        if is_ndk_root "$d"; then
          local ver; ver="$(basename "$d")"
          if [[ -z "$best" || "$ver" > "$best" ]]; then
            best="$ver"
            found="$d"
          fi
        fi
      done
    fi

    if [[ -n "${found}" ]]; then
      NDK_ROOT="${found%/}"
    elif [[ -d "${NDK_CACHE_DIR}" ]] && is_ndk_root "${NDK_CACHE_DIR}"; then
      NDK_ROOT="${NDK_CACHE_DIR}"
    else
      local default_dir="${NDK_CACHE_DIR}/android-ndk-${NDK_VERSION}"
      if [[ -d "${default_dir}" ]] && is_ndk_root "${default_dir}"; then
        NDK_ROOT="${default_dir}"
      else
        echo "NDK not found locally -> will download to ${default_dir}"
        download_ndk_to "${default_dir}"
      fi
    fi
  fi

  HOST_TAG="$(host_tag)"
  LLVM_BIN="${NDK_ROOT}/toolchains/llvm/prebuilt/${HOST_TAG}/bin"
  SYSROOT="${NDK_ROOT}/toolchains/llvm/prebuilt/${HOST_TAG}/sysroot"
  [[ -d "$LLVM_BIN" && -d "$SYSROOT" ]] || { echo "Invalid NDK: $NDK_ROOT"; exit 1; }

  STRIP_BIN="${LLVM_BIN}/llvm-strip"
  [[ -x "$STRIP_BIN" ]] || { echo "missing $STRIP_BIN"; exit 1; }

  print_ndk_info
  echo "Using HOST_TAG: ${HOST_TAG}"

  mk_wrap() {
    local wrapper="$1" target="$2"
    mkdir -p "$(dirname "$wrapper")"
    printf '%s\n' "#!/usr/bin/env bash" >"$wrapper"
    printf '%s\n' "exec \"${LLVM_BIN}/clang\" -target ${target} --sysroot=\"${SYSROOT}\" \"\$@\"" >>"$wrapper"
    chmod +x "$wrapper"
  }

  WRAP_DIR=".ndkwrap_sdk"
  rm -rf "${WRAP_DIR}"; mkdir -p "${WRAP_DIR}"

  AARCH64_CLANG="${LLVM_BIN}/aarch64-linux-android${API}-clang"
  X86_64_CLANG="${LLVM_BIN}/x86_64-linux-android${API}-clang"
  ARMV7A_CLANG="${LLVM_BIN}/armv7a-linux-androideabi${API}-clang"
  I686_CLANG="${LLVM_BIN}/i686-linux-android${API}-clang"

  [[ -x "$AARCH64_CLANG" ]] || { AARCH64_CLANG="${WRAP_DIR}/aarch64-linux-android${API}-clang";   mk_wrap "$AARCH64_CLANG" "aarch64-linux-android${API}";    }
  [[ -x "$X86_64_CLANG"  ]] || { X86_64_CLANG="${WRAP_DIR}/x86_64-linux-android${API}-clang";     mk_wrap "$X86_64_CLANG"  "x86_64-linux-android${API}";     }
  [[ -x "$ARMV7A_CLANG"  ]] || { ARMV7A_CLANG="${WRAP_DIR}/armv7a-linux-androideabi${API}-clang"; mk_wrap "$ARMV7A_CLANG"  "armv7a-linux-androideabi${API}"; }
  [[ -x "$I686_CLANG"    ]] || { I686_CLANG="${WRAP_DIR}/i686-linux-android${API}-clang";         mk_wrap "$I686_CLANG"    "i686-linux-android${API}";       }

  export LLVM_BIN STRIP_BIN SYSROOT
}

build_one() {
  local outdir="$1" goarch="$2" cc="$3" extra_env="${4:-}"
  mkdir -p "$OUT_ROOT/$outdir"
  local bin="$OUT_ROOT/$outdir/lib${APP_NAME}.so"
  echo "==> Building $outdir ($goarch) -> $bin"

  # SONAME 必须设置：否则 CMake 链接预编译库时会把宿主机绝对路径写入
  # libnpcjni.so 的 DT_NEEDED，导致 Android 上 dlopen 失败。
  local EXTLD="-Wl,-soname,lib${APP_NAME}.so -Wl,-z,max-page-size=16384"
  if "${LLVM_BIN}/ld.lld" --help 2>/dev/null | grep -q 'common-page-size'; then
    EXTLD+=" -Wl,-z,common-page-size=16384"
  fi

  local -a ENVV=(CGO_ENABLED=1 GOOS=android GOARCH="$goarch" CC="$cc")
  [[ -n "$extra_env" ]] && ENVV+=("$extra_env")

  env "${ENVV[@]}" \
    go build -trimpath -buildvcs=false -tags "$TAGS" \
      -buildmode=c-shared \
      -ldflags "-s -w -buildid= -linkmode=external -extldflags '${EXTLD}'" \
      -o "$bin" "$SRC"

  "$STRIP_BIN" -s "$bin" || true
}

main() {
  ensure_ndk
  build_one "arm64-v8a"   "arm64" "$AARCH64_CLANG"
  build_one "x86_64"      "amd64" "$X86_64_CLANG"
  build_one "armeabi-v7a" "arm"   "$ARMV7A_CLANG" "GOARM=7"
  build_one "x86"         "386"   "$I686_CLANG"

  echo ""
  echo "OK: $OUT_ROOT"
  echo ""
  for abi in arm64-v8a armeabi-v7a x86_64 x86; do
    local so="$OUT_ROOT/$abi/lib${APP_NAME}.so"
    if [[ -f "$so" ]]; then
      echo "--- $abi ---"
      file "$so"
    fi
  done
}

main "$@"
