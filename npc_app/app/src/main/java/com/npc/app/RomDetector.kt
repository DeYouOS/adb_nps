package com.npc.app

import android.os.Build
import android.util.Log
import java.io.BufferedReader
import java.io.InputStreamReader

/**
 * ROM family detection based on Build.MANUFACTURER / Build.BRAND
 * and system properties via `getprop`.
 *
 * Detection order matters: HyperOS must be checked before MIUI
 * because HyperOS devices may still have `ro.miui.ui.version.name` set.
 */
enum class RomFamily {
    MIUI,
    HYPEROS,
    COLOROS,
    ORIGINOS,
    FUNTOUCH,
    EMUI,
    HARMONY,
    MAGICOS,
    ONEUI,
    AOSP
}

object RomDetector {

    private const val TAG = "RomDetector"

    /** Detected ROM family (cached after first call). */
    val family: RomFamily by lazy { detect() }

    /** Human-readable ROM name for display, e.g. "小米 HyperOS". */
    val romDisplayName: String by lazy {
        when (family) {
            RomFamily.MIUI -> "小米 MIUI"
            RomFamily.HYPEROS -> "小米 HyperOS"
            RomFamily.COLOROS -> "OPPO ColorOS"
            RomFamily.ORIGINOS -> "vivo OriginOS"
            RomFamily.FUNTOUCH -> "vivo Funtouch"
            RomFamily.EMUI -> "华为 EMUI"
            RomFamily.HARMONY -> "华为 HarmonyOS"
            RomFamily.MAGICOS -> "荣耀 MagicOS"
            RomFamily.ONEUI -> "三星 One UI"
            RomFamily.AOSP -> "Android (原生)"
        }
    }

    /** Android version string for display. */
    val androidVersion: String
        get() = "Android ${Build.VERSION.RELEASE} (API ${Build.VERSION.SDK_INT})"

    /** Device model for display. */
    val deviceModel: String
        get() = "${Build.MANUFACTURER} ${Build.MODEL}"

    private fun detect(): RomFamily {
        val mfr = Build.MANUFACTURER.lowercase()
        val brand = Build.BRAND.lowercase()

        // Samsung — check first (unambiguous)
        if (mfr == "samsung" || brand == "samsung") {
            val oneui = getprop("ro.build.version.oneui")
            if (!oneui.isNullOrEmpty()) {
                Log.d(TAG, "Detected Samsung One UI (oneui=$oneui)")
                return RomFamily.ONEUI
            }
            // Older Samsung without One UI prop — still treat as ONEUI
            Log.d(TAG, "Detected Samsung (no oneui prop)")
            return RomFamily.ONEUI
        }

        // Huawei / Honor — must distinguish EMUI vs HarmonyOS vs MagicOS
        if (mfr == "huawei" || brand == "huawei") {
            // Check Honor first (some old Honor devices have manufacturer=huawei)
            val honorId = getprop("ro.honor.build.display.id")
            val magicVer = getprop("ro.build.version.magic")
            if (!honorId.isNullOrEmpty() || !magicVer.isNullOrEmpty()) {
                Log.d(TAG, "Detected Honor MagicOS (honorId=$honorId, magic=$magicVer)")
                return RomFamily.MAGICOS
            }
            // HarmonyOS: check hw_sc.build.platform.version
            val harmonyVer = getprop("hw_sc.build.platform.version")
            if (!harmonyVer.isNullOrEmpty()) {
                Log.d(TAG, "Detected HarmonyOS (hw_sc=$harmonyVer)")
                return RomFamily.HARMONY
            }
            // EMUI
            val emuiVer = getprop("ro.build.version.emui")
            if (!emuiVer.isNullOrEmpty()) {
                Log.d(TAG, "Detected EMUI ($emuiVer)")
                return RomFamily.EMUI
            }
            // Fallback: treat as EMUI
            Log.d(TAG, "Huawei device, fallback to EMUI")
            return RomFamily.EMUI
        }

        // Honor (Android 12+ with brand=honor)
        if (mfr == "honor" || brand == "honor") {
            Log.d(TAG, "Detected Honor MagicOS")
            return RomFamily.MAGICOS
        }

        // Xiaomi — HyperOS vs MIUI (HyperOS check FIRST)
        if (mfr == "xiaomi" || brand == "xiaomi" || brand == "redmi" || brand == "poco") {
            val hyperOs = getprop("ro.mi.os.version.name")
            if (!hyperOs.isNullOrEmpty()) {
                Log.d(TAG, "Detected HyperOS ($hyperOs)")
                return RomFamily.HYPEROS
            }
            val miuiVer = getprop("ro.miui.ui.version.name")
            if (!miuiVer.isNullOrEmpty()) {
                Log.d(TAG, "Detected MIUI ($miuiVer)")
                return RomFamily.MIUI
            }
            // Xiaomi device without MIUI/HyperOS props — assume HyperOS (newer devices)
            Log.d(TAG, "Xiaomi device without MIUI/HyperOS props, assuming HyperOS")
            return RomFamily.HYPEROS
        }

        // OPPO / realme / OnePlus
        if (mfr == "oppo" || brand == "oppo" || mfr == "realme" || brand == "realme"
            || mfr == "oneplus" || brand == "oneplus") {
            val colorNew = getprop("ro.build.version.oplusrom")
            val colorOld = getprop("ro.build.version.opporom")
            if (!colorNew.isNullOrEmpty() || !colorOld.isNullOrEmpty()) {
                Log.d(TAG, "Detected ColorOS (new=$colorNew, old=$colorOld)")
                return RomFamily.COLOROS
            }
            // OPPO-family without props — assume ColorOS
            Log.d(TAG, "OPPO-family device, assuming ColorOS")
            return RomFamily.COLOROS
        }

        // vivo / iQOO
        if (mfr == "vivo" || brand == "vivo" || brand == "iqoo") {
            val vivoOs = getprop("ro.vivo.os.version")
            val displayId = getprop("ro.build.display.id") ?: ""
            // OriginOS: vivo.os.version contains "Origin" or display.id contains "Origin"
            if (!vivoOs.isNullOrEmpty() && vivoOs.contains("origin", ignoreCase = true)) {
                Log.d(TAG, "Detected OriginOS ($vivoOs)")
                return RomFamily.ORIGINOS
            }
            if (displayId.contains("origin", ignoreCase = true)) {
                Log.d(TAG, "Detected OriginOS (displayId=$displayId)")
                return RomFamily.ORIGINOS
            }
            // If vivo OS version exists but not Origin → Funtouch
            if (!vivoOs.isNullOrEmpty()) {
                Log.d(TAG, "Detected Funtouch ($vivoOs)")
                return RomFamily.FUNTOUCH
            }
            // vivo without props — check if newer (OriginOS) or older (Funtouch)
            // Assume OriginOS for Android 11+ vivo devices
            if (Build.VERSION.SDK_INT >= 30) {
                Log.d(TAG, "vivo Android 11+, assuming OriginOS")
                return RomFamily.ORIGINOS
            }
            Log.d(TAG, "vivo older device, assuming Funtouch")
            return RomFamily.FUNTOUCH
        }

        // AOSP / other
        Log.d(TAG, "No known ROM detected, using AOSP fallback")
        return RomFamily.AOSP
    }

    /**
     * Read a system property via `getprop`. Returns null if the property
     * is empty or the command fails.
     */
    fun getprop(key: String): String? {
        return try {
            val process = Runtime.getRuntime().exec(arrayOf("getprop", key))
            val reader = BufferedReader(InputStreamReader(process.inputStream))
            val value = reader.readLine()?.trim()
            reader.close()
            process.waitFor()
            if (value.isNullOrEmpty()) null else value
        } catch (e: Exception) {
            Log.w(TAG, "getprop($key) failed: ${e.message}")
            null
        }
    }
}
