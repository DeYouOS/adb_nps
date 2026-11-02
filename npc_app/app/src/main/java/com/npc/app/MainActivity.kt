package com.npc.app

import android.Manifest
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.os.PowerManager
import android.provider.Settings
import android.util.Log
import android.view.View
import android.view.WindowManager
import android.widget.Button
import android.widget.ScrollView
import android.widget.Switch
import android.widget.TextView
import androidx.appcompat.app.AppCompatActivity
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat
import kotlin.concurrent.thread

class MainActivity : AppCompatActivity() {

    companion object {
        private const val TAG = "MainActivity"
        private const val REQUEST_OVERLAY = 1001
        private const val REQUEST_NOTIFICATION = 1002
    }

    private lateinit var tvVersion: TextView
    private lateinit var tvConfig: TextView
    private lateinit var tvStatus: TextView
    private lateinit var tvLogs: TextView
    private lateinit var tvPermStatus: TextView
    private lateinit var scrollLogs: ScrollView
    private lateinit var btnLogs: Button
    private lateinit var btnRestart: Button
    private lateinit var btnExit: Button
    private lateinit var btnPermCheck: Button
    private lateinit var switchKeepScreen: Switch

    private val handler = Handler(Looper.getMainLooper())
    private var config: NpcConfig? = null
    private var floatingWindow: FloatingWindow? = null
    private val logAccumulator = StringBuilder()

    private val statusReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context, intent: Intent) {
            val code = intent.getIntExtra(NpcService.EXTRA_STATUS_CODE, 0)
            val text = intent.getStringExtra(NpcService.EXTRA_STATUS_TEXT) ?: "Unknown"
            updateStatusUI(code, text)
            floatingWindow?.updateStatus(
                code == NpcService.STATUS_CONNECTED,
                if (code == NpcService.STATUS_CONNECTED) "NPC ✓" else "NPC ✗"
            )
        }
    }

    // Log poller — runs every 2s to accumulate logs
    private val logPoller = object : Runnable {
        override fun run() {
            if (!NpcService.isRunning) {
                handler.postDelayed(this, 2000)
                return
            }
            try {
                val newLogs = NpcNative.logs()
                if (newLogs.isNotEmpty()) {
                    logAccumulator.append(NpcMask.hide(newLogs, config?.server))
                    if (logAccumulator.length > 65536) {
                        logAccumulator.delete(0, logAccumulator.length - 32768)
                    }
                    if (scrollLogs.visibility == View.VISIBLE) {
                        tvLogs.text = NpcMask.hide(logAccumulator.toString(), config?.server)
                        scrollLogs.post { scrollLogs.fullScroll(View.FOCUS_DOWN) }
                    }
                }
            } catch (_: Exception) {}
            handler.postDelayed(this, 2000)
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)

        tvVersion = findViewById(R.id.tv_version)
        tvConfig = findViewById(R.id.tv_config)
        tvStatus = findViewById(R.id.tv_status)
        tvLogs = findViewById(R.id.tv_logs)
        tvPermStatus = findViewById(R.id.tv_perm_status)
        scrollLogs = findViewById(R.id.scroll_logs)
        btnLogs = findViewById(R.id.btn_logs)
        btnRestart = findViewById(R.id.btn_restart)
        btnExit = findViewById(R.id.btn_exit)
        btnPermCheck = findViewById(R.id.btn_perm_check)
        switchKeepScreen = findViewById(R.id.switch_keep_screen)

        config = loadConfig()
        displayConfig()

        // Show native version
        thread {
            val ver = try { NpcNative.version() } catch (_: Exception) { "unknown" }
            runOnUiThread { tvVersion.text = "NPC v$ver" }
        }

        btnLogs.setOnClickListener { toggleLogs() }
        btnRestart.setOnClickListener { restartClient() }
        btnExit.setOnClickListener { exitApp() }
        btnPermCheck.setOnClickListener { openPermGuide() }

        // Keep screen on toggle
        val prefs = getSharedPreferences("npc_prefs", Context.MODE_PRIVATE)
        switchKeepScreen.isChecked = prefs.getBoolean("keep_screen_on", false)
        applyKeepScreenOn(switchKeepScreen.isChecked)
        switchKeepScreen.setOnCheckedChangeListener { _, isChecked ->
            prefs.edit().putBoolean("keep_screen_on", isChecked).apply()
            applyKeepScreenOn(isChecked)
        }

        // Floating window
        floatingWindow = FloatingWindow(this).apply {
            onClickListener = {
                val intent = Intent(this@MainActivity, MainActivity::class.java).apply {
                    flags = Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP
                }
                startActivity(intent)
            }
        }

        // Permission-gated service start
        if (isConfigValid(config)) {
            if (PermissionUtils.allSatisfied(this, prefs)) {
                startNpcService()
            } else {
                // Not all permissions satisfied — launch guide
                tvStatus.text = "Status: 等待权限设置"
                startActivity(Intent(this, PermissionGuideActivity::class.java))
            }
        } else {
            tvStatus.text = "Status: No valid config"
        }

        // Register status broadcast
        val filter = IntentFilter(NpcService.BROADCAST_STATUS)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            registerReceiver(statusReceiver, filter, Context.RECEIVER_NOT_EXPORTED)
        } else {
            registerReceiver(statusReceiver, filter)
        }

        handler.post(logPoller)
    }

    override fun onResume() {
        super.onResume()
        updatePermStatusLine()
        if (Settings.canDrawOverlays(this)) {
            floatingWindow?.show()
        }

        // If all permissions are now satisfied and service is not running, start it
        val prefs = getSharedPreferences("npc_prefs", Context.MODE_PRIVATE)
        if (PermissionUtils.allSatisfied(this, prefs)
            && !NpcService.isRunning
            && isConfigValid(config)
        ) {
            startNpcService()
        }
    }

    override fun onDestroy() {
        handler.removeCallbacks(logPoller)
        try { unregisterReceiver(statusReceiver) } catch (_: Exception) {}
        super.onDestroy()
    }

    private fun loadConfig(): NpcConfig? {
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
            Log.e(TAG, "Failed to load config", e)
            null
        }
    }

    private fun isConfigValid(cfg: NpcConfig?): Boolean {
        return cfg != null && cfg.server.isNotEmpty() && cfg.vkey.isNotEmpty()
    }

    private fun displayConfig() {
        val cfg = config
        if (cfg == null) {
            tvConfig.text = "Config: Failed to load npc_config.json"
            return
        }
        tvConfig.text = buildString {
            append("Remark: ${cfg.remark.ifEmpty { "-" }}\n")
            append("Client ID: ${cfg.clientId}\n")
            append("Type: ${cfg.connType}")
        }
    }

    private fun updateStatusUI(code: Int, text: String) {
        val icon = when (code) {
            NpcService.STATUS_CONNECTED -> "●"
            NpcService.STATUS_ERROR -> "✖"
            else -> "○"
        }
        tvStatus.text = "Status: $icon $text"
    }

    private fun startNpcService() {
        val cfg = config ?: return
        val intent = Intent(this, NpcService::class.java).apply {
            action = NpcService.ACTION_START
            putExtra(NpcService.EXTRA_SERVER, cfg.server)
            putExtra(NpcService.EXTRA_VKEY, cfg.vkey)
            putExtra(NpcService.EXTRA_CONN_TYPE, cfg.connType)
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            startForegroundService(intent)
        } else {
            startService(intent)
        }
        tvStatus.text = "Status: Starting..."
    }

    private fun restartClient() {
        logAccumulator.clear()
        tvLogs.text = ""
        val stopIntent = Intent(this, NpcService::class.java).apply {
            action = NpcService.ACTION_STOP
        }
        startService(stopIntent)
        handler.postDelayed({ startNpcService() }, 500)
    }

    private fun exitApp() {
        val stopIntent = Intent(this, NpcService::class.java).apply {
            action = NpcService.ACTION_STOP
        }
        startService(stopIntent)
        floatingWindow?.hide()
        finishAndRemoveTask()
    }

    private fun toggleLogs() {
        if (scrollLogs.visibility == View.VISIBLE) {
            scrollLogs.visibility = View.GONE
            btnLogs.text = "Show Logs"
        } else {
            scrollLogs.visibility = View.VISIBLE
            btnLogs.text = "Hide Logs"
            tvLogs.text = NpcMask.hide(logAccumulator.toString(), config?.server)
            scrollLogs.post { scrollLogs.fullScroll(View.FOCUS_DOWN) }
        }
    }

    private fun openPermGuide() {
        startActivity(Intent(this, PermissionGuideActivity::class.java))
    }

    /**
     * Update the permission status line on the main screen.
     * Shows e.g. "权限: 3/4 已就绪"
     */
    private fun updatePermStatusLine() {
        val prefs = getSharedPreferences("npc_prefs", Context.MODE_PRIVATE)
        val satisfied = PermissionUtils.countSatisfied(this, prefs)
        val total = PermKind.values().size
        tvPermStatus.text = "权限: $satisfied/$total 已就绪"
    }

    private fun applyKeepScreenOn(enabled: Boolean) {
        if (enabled) {
            window.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        } else {
            window.clearFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        }
        floatingWindow?.setKeepScreenOn(enabled)
    }
}
