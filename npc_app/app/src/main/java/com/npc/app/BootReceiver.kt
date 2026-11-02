package com.npc.app

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.os.Build
import android.util.Log

/**
 * Restores [NpcService] after a device reboot.
 *
 * Receives ACTION_BOOT_COMPLETED (plus MIUI / HTC quick-boot variants).
 *
 * Legality note (Android 12+):
 * ACTION_BOOT_COMPLETED is an explicit exemption listed in the Android docs for
 * starting foreground services from the background — no additional workaround
 * is needed.  See:
 * https://developer.android.com/about/versions/12/foreground-services#exemptions
 *
 * The receiver is intentionally lightweight: it reads the bundled asset config
 * and SharedPreferences (both are local I/O, no network), decides whether to
 * start, and returns immediately.
 */
class BootReceiver : BroadcastReceiver() {

    companion object {
        private const val TAG = "BootReceiver"
    }

    override fun onReceive(context: Context, intent: Intent) {
        val action = intent.action ?: return
        if (action != Intent.ACTION_BOOT_COMPLETED &&
            action != "android.intent.action.QUICKBOOT_POWERON" &&
            action != "com.htc.intent.action.QUICKBOOT_POWERON"
        ) {
            return
        }

        Log.i(TAG, "Boot completed, evaluating whether to restore tunnel service")

        try {
            // 1. Read bundled config (same way NpcService / MainActivity do it)
            val cfg = readConfig(context)
            if (cfg == null || cfg.server.isEmpty() || cfg.vkey.isEmpty()) {
                Log.i(TAG, "No valid config (server/vkey empty) — skipping auto-start")
                return
            }

            // 2. Check that every required permission is still satisfied
            val prefs = context.getSharedPreferences("npc_prefs", Context.MODE_PRIVATE)
            if (!PermissionUtils.allSatisfied(context, prefs)) {
                Log.i(TAG, "Not all permissions satisfied — skipping auto-start")
                return
            }

            // 3. Start NpcService as a foreground service (same as MainActivity)
            // ACTION_BOOT_COMPLETED is a background-start exemption on Android 12+,
            // so startForegroundService is legal here without any special workaround.
            val serviceIntent = Intent(context, NpcService::class.java)
            serviceIntent.setAction(NpcService.ACTION_START)
            serviceIntent.putExtra(NpcService.EXTRA_SERVER, cfg.server)
            serviceIntent.putExtra(NpcService.EXTRA_VKEY, cfg.vkey)
            serviceIntent.putExtra(NpcService.EXTRA_CONN_TYPE, cfg.connType)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                context.startForegroundService(serviceIntent)
            } else {
                context.startService(serviceIntent)
            }
            Log.i(TAG, "NpcService start requested (server=${NpcMask.hide(cfg.server, cfg.server)})")
        } catch (e: Exception) {
            Log.e(TAG, "Failed to restore tunnel service on boot", e)
        }
    }

    /**
     * Reads [NpcConfig] from assets/npc_config.json — mirrors [NpcService.readConfig].
     */
    private fun readConfig(context: Context): NpcConfig? {
        return try {
            val json = context.assets.open("npc_config.json").bufferedReader().readText()
            val obj = org.json.JSONObject(json)
            NpcConfig(
                server = obj.optString("server", ""),
                vkey = obj.optString("vkey", ""),
                connType = obj.optString("conn_type", "tcp"),
                clientId = obj.optInt("client_id", 0),
                remark = obj.optString("remark", "")
            )
        } catch (e: Exception) {
            Log.e(TAG, "readConfig error", e)
            null
        }
    }
}
