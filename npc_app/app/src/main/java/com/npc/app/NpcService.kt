package com.npc.app

import android.app.AlarmManager
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.Build
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.net.wifi.WifiManager
import android.os.PowerManager
import android.util.Log
import androidx.core.app.NotificationCompat
import kotlin.concurrent.thread

class NpcService : Service() {

    companion object {
        const val TAG = "NpcService"
        const val CHANNEL_ID = "npc_service"
        const val NOTIFICATION_ID = 1
        const val ACTION_START = "com.npc.app.ACTION_START"
        const val ACTION_STOP = "com.npc.app.ACTION_STOP"

        const val EXTRA_SERVER = "server"
        const val EXTRA_VKEY = "vkey"
        const val EXTRA_CONN_TYPE = "conn_type"

        const val STATUS_DISCONNECTED = 0
        const val STATUS_CONNECTED = 1
        const val STATUS_ERROR = -1

        const val BROADCAST_STATUS = "com.npc.app.STATUS_UPDATE"
        const val EXTRA_STATUS_CODE = "status_code"
        const val EXTRA_STATUS_TEXT = "status_text"

        var isRunning = false
            private set
    }

    private val handler = Handler(Looper.getMainLooper())
    private var connected = false

    private var wakeLock: PowerManager.WakeLock? = null
    private var wifiLock: WifiManager.WifiLock? = null
    private var screenWakeLock: PowerManager.WakeLock? = null
    private var serverAddr: String = ""

    private val statusPoller = object : Runnable {
        override fun run() {
            try {
                val status = NpcNative.getStatus()
                val nowConnected = status == 1
                if (nowConnected != connected) {
                    connected = nowConnected
                    updateNotification(if (connected) "Connected" else "Disconnected")
                    sendStatusBroadcast(
                        if (connected) STATUS_CONNECTED else STATUS_DISCONNECTED,
                        if (connected) "Connected" else "Disconnected"
                    )
                }
                // Accumulate logs (logs() clears the buffer)
                val newLogs = try { NpcNative.logs() } catch (_: Exception) { "" }
                if (newLogs.isNotEmpty()) {
                    Log.d(TAG, "logs: ${NpcMask.hide(newLogs.take(200), serverAddr)}")
                }
            } catch (e: Exception) {
                Log.e(TAG, "Status poll error", e)
            }
            handler.postDelayed(this, 1000)
        }
    }

    override fun onCreate() {
        super.onCreate()
        createNotificationChannel()
        isRunning = true
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> {
                stopClient()
                stopForeground(STOP_FOREGROUND_REMOVE)
                stopSelf()
                return START_NOT_STICKY
            }
            ACTION_START -> {
                val server = intent.getStringExtra(EXTRA_SERVER) ?: ""
                val vkey = intent.getStringExtra(EXTRA_VKEY) ?: ""
                val connType = intent.getStringExtra(EXTRA_CONN_TYPE) ?: "tcp"
                startForeground(NOTIFICATION_ID, buildNotification("Starting..."))
                startClient(server, vkey, connType)
            }
            else -> {
                // Restarted by system (START_STICKY)
                startForeground(NOTIFICATION_ID, buildNotification("Restarting..."))
                restartFromConfig()
            }
        }
        return START_STICKY
    }

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onTaskRemoved(rootIntent: Intent?) {
        super.onTaskRemoved(rootIntent)
        // Restart on task removal for survivability
        val restartIntent = Intent(applicationContext, NpcService::class.java).apply {
            action = ACTION_START
        }
        val pendingIntent = PendingIntent.getService(
            applicationContext, 1, restartIntent,
            PendingIntent.FLAG_ONE_SHOT or PendingIntent.FLAG_IMMUTABLE
        )
        val alarmManager = getSystemService(Context.ALARM_SERVICE) as AlarmManager
        alarmManager.set(
            AlarmManager.RTC_WAKEUP,
            System.currentTimeMillis() + 1000,
            pendingIntent
        )
    }

    override fun onDestroy() {
        handler.removeCallbacks(statusPoller)
        stopClient()
        releaseWakeLocks()
        isRunning = false
        super.onDestroy()
    }

    private fun acquireWakeLocks() {
        // PARTIAL_WAKE_LOCK — keeps CPU alive with screen off
        val pm = getSystemService(Context.POWER_SERVICE) as PowerManager
        wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "npc:client").apply {
            setReferenceCounted(false)
            acquire()
        }
        Log.d(TAG, "PARTIAL_WAKE_LOCK acquired")

        // WifiLock — keeps WiFi radio alive
        try {
            val wm = applicationContext.getSystemService(Context.WIFI_SERVICE) as WifiManager
            val mode = if (Build.VERSION.SDK_INT >= 29)
                WifiManager.WIFI_MODE_FULL_LOW_LATENCY
            else
                WifiManager.WIFI_MODE_FULL_HIGH_PERF
            wifiLock = wm.createWifiLock(mode, "npc:wifi").apply {
                setReferenceCounted(false)
                acquire()
            }
            Log.d(TAG, "WifiLock acquired (mode=$mode)")
        } catch (e: Exception) {
            Log.e(TAG, "Failed to acquire WifiLock", e)
        }

        // Optional screen-on wake lock
        updateScreenWakeLock()
    }

    private fun releaseWakeLocks() {
        wakeLock?.let { if (it.isHeld) it.release() }
        wakeLock = null
        wifiLock?.let { if (it.isHeld) it.release() }
        wifiLock = null
        releaseScreenWakeLock()
        Log.d(TAG, "Wake/Wifi locks released")
    }

    fun updateScreenWakeLock() {
        val prefs = getSharedPreferences("npc_prefs", Context.MODE_PRIVATE)
        val keepScreenOn = prefs.getBoolean("keep_screen_on", false)
        if (keepScreenOn) {
            if (screenWakeLock == null) {
                val pm = getSystemService(Context.POWER_SERVICE) as PowerManager
                screenWakeLock = pm.newWakeLock(
                    PowerManager.SCREEN_DIM_WAKE_LOCK or PowerManager.ACQUIRE_CAUSES_WAKEUP,
                    "npc:screen"
                ).apply {
                    setReferenceCounted(false)
                    acquire()
                }
                Log.d(TAG, "SCREEN_DIM_WAKE_LOCK acquired")
            }
        } else {
            releaseScreenWakeLock()
        }
    }

    private fun releaseScreenWakeLock() {
        screenWakeLock?.let { if (it.isHeld) it.release() }
        screenWakeLock = null
    }

    private fun startClient(server: String, vkey: String, connType: String) {
        serverAddr = server
        acquireWakeLocks()
        thread {
            try {
                NpcNative.setLogsLevel("info")
                val ret = NpcNative.startClient(server, vkey, connType, null)
                if (ret == 1) {
                    handler.post(statusPoller)
                    sendStatusBroadcast(STATUS_CONNECTED, "Connecting...")
                } else {
                    updateNotification("Start failed ($ret)")
                    sendStatusBroadcast(STATUS_ERROR, "Start failed ($ret)")
                }
            } catch (e: Exception) {
                Log.e(TAG, "startClient error", e)
                updateNotification("Error: ${e.message}")
                sendStatusBroadcast(STATUS_ERROR, "Error: ${e.message}")
            }
        }
    }

    private fun stopClient() {
        releaseWakeLocks()
        handler.removeCallbacks(statusPoller)
        thread {
            try {
                NpcNative.closeClient()
            } catch (e: Exception) {
                Log.e(TAG, "closeClient error", e)
            }
        }
        connected = false
        sendStatusBroadcast(STATUS_DISCONNECTED, "Disconnected")
    }

    private fun restartFromConfig() {
        thread {
            try {
                val cfg = readConfig()
                if (cfg != null && cfg.server.isNotEmpty() && cfg.vkey.isNotEmpty()) {
                    startClient(cfg.server, cfg.vkey, cfg.connType)
                } else {
                    updateNotification("No valid config")
                    sendStatusBroadcast(STATUS_ERROR, "No valid config")
                }
            } catch (e: Exception) {
                Log.e(TAG, "restartFromConfig error", e)
                updateNotification("Config error")
            }
        }
    }

    private fun readConfig(): NpcConfig? {
        return try {
            val json = assets.open("npc_config.json").bufferedReader().readText()
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

    private fun createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL_ID,
                "NPC Service",
                NotificationManager.IMPORTANCE_LOW
            ).apply {
                description = "NPC client connection service"
                setShowBadge(false)
            }
            val nm = getSystemService(NotificationManager::class.java)
            nm.createNotificationChannel(channel)
        }
    }

    private fun buildNotification(status: String): Notification {
        val stopIntent = Intent(this, NpcService::class.java).apply { action = ACTION_STOP }
        val stopPending = PendingIntent.getService(
            this, 0, stopIntent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )

        val openIntent = Intent(this, MainActivity::class.java).apply {
            flags = Intent.FLAG_ACTIVITY_SINGLE_TOP
        }
        val openPending = PendingIntent.getActivity(
            this, 0, openIntent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )

        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setContentTitle("NPC Client")
            .setContentText(status)
            .setSmallIcon(android.R.drawable.ic_menu_share)
            .setOngoing(true)
            .setContentIntent(openPending)
            .addAction(android.R.drawable.ic_menu_close_clear_cancel, "Stop", stopPending)
            .build()
    }

    private fun updateNotification(status: String) {
        val nm = getSystemService(NotificationManager::class.java)
        nm.notify(NOTIFICATION_ID, buildNotification(status))
    }

    private fun sendStatusBroadcast(statusCode: Int, statusText: String) {
        val intent = Intent(BROADCAST_STATUS).apply {
            putExtra(EXTRA_STATUS_CODE, statusCode)
            putExtra(EXTRA_STATUS_TEXT, statusText)
            setPackage(packageName)
        }
        sendBroadcast(intent)
    }
}

data class NpcConfig(
    val server: String,
    val vkey: String,
    val connType: String,
    val clientId: Int,
    val remark: String
)

object NpcMask {
    fun hide(text: String, server: String?): String {
        if (server.isNullOrEmpty()) return text
        var out = text
        out = out.replace(server, "***")
        val host = server.substringBefore(":")
        if (host.isNotEmpty()) out = out.replace(host, "***")
        return out
    }
}
