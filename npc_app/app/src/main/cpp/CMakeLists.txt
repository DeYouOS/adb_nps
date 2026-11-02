cmake_minimum_required(VERSION 3.22)
project(npcjni C)

# Prebuilt Go c-shared library (compiled by build.android.sdk.sh)
add_library(npc SHARED IMPORTED)
set_target_properties(npc PROPERTIES
    IMPORTED_LOCATION ${CMAKE_SOURCE_DIR}/../jniLibs/${ANDROID_ABI}/libnpc.so
    # 双保险：即使 libnpc.so 缺少 SONAME，也按 -lnpc 链接，
    # 避免把宿主机绝对路径写入 DT_NEEDED
    IMPORTED_NO_SONAME TRUE
)

# JNI wrapper that bridges Java <-> Go
add_library(npcjni SHARED npc_jni.c)

# Link against the prebuilt Go library and Android log
target_link_libraries(npcjni npc log)
