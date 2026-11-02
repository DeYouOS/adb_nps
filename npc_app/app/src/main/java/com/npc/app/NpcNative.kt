package com.npc.app

object NpcNative {
    init {
        System.loadLibrary("npc")      // Go c-shared library (must load first — dependency)
        System.loadLibrary("npcjni")   // JNI wrapper (depends on libnpc.so)
    }

    external fun startClient(server: String, vkey: String, connType: String, proxy: String?): Int
    external fun getStatus(): Int
    external fun closeClient()
    external fun version(): String
    external fun logs(): String
    external fun setLogsLevel(level: String)
    external fun setDnsServer(dns: String)
}
