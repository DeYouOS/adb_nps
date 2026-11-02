package com.npc.app

import android.Manifest
import android.content.Context
import android.content.Intent
import android.content.SharedPreferences
import android.content.pm.PackageManager
import android.graphics.Color
import android.graphics.drawable.GradientDrawable
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import android.util.Log
import android.view.LayoutInflater
import android.view.View
import android.widget.Button
import android.widget.LinearLayout
import android.widget.TextView
import androidx.appcompat.app.AppCompatActivity
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat

/**
 * Permission guide Activity — the first thing the user sees after install.
 *
 * Shows 4 permission cards (OVERLAY, NOTIFICATION, BATTERY, AUTOSTART)
 * with ROM-specific guidance. The primary button is only enabled when
 * all permissions are satisfied, at which point it starts NpcService.
 *
 * Key behaviors:
 * - onResume re-evaluates all states
 * - BATTERY gets a delayed re-check (1200ms) for HyperOS/MIUI A14
 * - NOTIFICATION triggers the real system dialog first on A13+
 * - AUTOSTART has a manual "我已设置好" confirmation button
 */
class PermissionGuideActivity : AppCompatActivity() {

    companion object {
        private const val TAG = "PermGuideActivity"
        private const val BATTERY_RECHECK_DELAY_MS = 1200L
    }

    private lateinit var tvDeviceInfo: TextView
    private lateinit var llCards: LinearLayout
    private lateinit var btnRecheck: Button
    private lateinit var btnStartService: Button

    private lateinit var prefs: SharedPreferences
    private val handler = Handler(Looper.getMainLooper())

    // Card views for each permission
    private val cardViews = mutableMapOf<PermKind, View>()

    // Steps expanded state
    private val stepsExpanded = mutableMapOf<PermKind, Boolean>()

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_permission_guide)

        tvDeviceInfo = findViewById(R.id.tv_device_info)
        llCards = findViewById(R.id.ll_cards)
        btnRecheck = findViewById(R.id.btn_recheck)
        btnStartService = findViewById(R.id.btn_start_service)

        prefs = getSharedPreferences("npc_prefs", Context.MODE_PRIVATE)

        // Header: device info
        tvDeviceInfo.text = "${RomDetector.romDisplayName} · ${RomDetector.androidVersion}\n${RomDetector.deviceModel}"

        // Create permission cards
        createPermissionCards()

        // Button handlers
        btnRecheck.setOnClickListener { refreshAllCards() }
        btnStartService.setOnClickListener { onStartServiceClicked() }

        // Initial state
        refreshAllCards()
    }

    override fun onResume() {
        super.onResume()
        // Re-evaluate all states when returning from settings
        refreshAllCards()

        // BATTERY: HyperOS/MIUI A14 needs ~1000ms before new state is readable
        // Schedule a delayed re-check
        handler.postDelayed({
            refreshAllCards()
        }, BATTERY_RECHECK_DELAY_MS)
    }

    override fun onDestroy() {
        handler.removeCallbacksAndMessages(null)
        super.onDestroy()
    }

    // ─────────────────────────────────────────────
    // Card creation
    // ─────────────────────────────────────────────

    private fun createPermissionCards() {
        llCards.removeAllViews()
        cardViews.clear()
        stepsExpanded.clear()

        for (kind in PermKind.values()) {
            val guide = PermissionGuides.guideFor(kind, this)
            val cardView = createCardView(guide)
            cardViews[kind] = cardView
            stepsExpanded[kind] = false
            llCards.addView(cardView)
        }
    }

    private fun createCardView(guide: PermGuide): View {
        val card = LayoutInflater.from(this).inflate(R.layout.item_permission_card, llCards, false)

        val tvTitle = card.findViewById<TextView>(R.id.tv_perm_title)
        val tvPurpose = card.findViewById<TextView>(R.id.tv_perm_purpose)
        val btnGoSettings = card.findViewById<Button>(R.id.btn_go_settings)
        val btnAutostartConfirm = card.findViewById<Button>(R.id.btn_autostart_confirm)
        val tvToggleSteps = card.findViewById<TextView>(R.id.tv_toggle_steps)
        val llSteps = card.findViewById<LinearLayout>(R.id.ll_steps)
        val tvMenuPath = card.findViewById<TextView>(R.id.tv_menu_path)
        val tvStepsDetail = card.findViewById<TextView>(R.id.tv_steps_detail)

        // Title and purpose
        tvTitle.text = guide.title
        tvPurpose.text = guide.purpose

        // Menu path and steps
        tvMenuPath.text = "菜单路径：${guide.menuPath}"
        val stepsText = guide.steps.mapIndexed { index, step ->
            "${index + 1}. $step"
        }.joinToString("\n")
        tvStepsDetail.text = stepsText

        // AUTOSTART: show "我已设置好" button
        if (guide.kind == PermKind.AUTOSTART && guide.detectable) {
            btnAutostartConfirm.visibility = View.VISIBLE
            btnAutostartConfirm.setOnClickListener {
                PermissionUtils.confirmAutostart(prefs)
                refreshAllCards()
            }
        }

        // If not detectable (AOSP autostart), hide "去设置" button
        if (!guide.detectable) {
            btnGoSettings.visibility = View.GONE
            btnAutostartConfirm.visibility = View.GONE
        }

        // "去设置" button
        btnGoSettings.setOnClickListener {
            if (guide.kind == PermKind.NOTIFICATION) {
                // On A13+, try the runtime permission dialog first
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
                    if (ContextCompat.checkSelfPermission(
                            this, Manifest.permission.POST_NOTIFICATIONS
                        ) != PackageManager.PERMISSION_GRANTED
                    ) {
                        PermissionUtils.requestNotificationPermissionIfNeeded(this)
                        return@setOnClickListener
                    }
                }
            }
            // Launch first resolvable intent
            PermissionUtils.launchFirstResolvable(this, guide.candidates)
        }

        // Toggle steps visibility
        tvToggleSteps.setOnClickListener {
            val expanded = stepsExpanded[guide.kind] ?: false
            stepsExpanded[guide.kind] = !expanded
            llSteps.visibility = if (expanded) View.GONE else View.VISIBLE
            tvToggleSteps.text = if (expanded) "查看步骤 ▼" else "收起步骤 ▲"
        }

        // Tag the card with its kind for later reference
        card.tag = guide.kind

        return card
    }

    // ─────────────────────────────────────────────
    // State refresh
    // ─────────────────────────────────────────────

    private fun refreshAllCards() {
        var satisfiedCount = 0
        for (kind in PermKind.values()) {
            val satisfied = PermissionUtils.check(kind, this, prefs)
            if (satisfied) satisfiedCount++
            updateCardBadge(kind, satisfied)
        }
        updateStartButton(satisfiedCount)
    }

    private fun updateCardBadge(kind: PermKind, satisfied: Boolean) {
        val cardView = cardViews[kind] ?: return
        val tvBadge = cardView.findViewById<TextView>(R.id.tv_perm_badge)
        val btnGoSettings = cardView.findViewById<Button>(R.id.btn_go_settings)
        val btnAutostartConfirm = cardView.findViewById<Button>(R.id.btn_autostart_confirm)

        val guide = PermissionGuides.guideFor(kind, this)

        if (!guide.detectable) {
            // Not detectable (AOSP autostart) — show as "无需设置"
            tvBadge.text = "✅ 无需设置"
            tvBadge.setTextColor(Color.parseColor("#2E7D32"))
            tvBadge.background = createBadgeBackground(Color.parseColor("#E8F5E9"))
            btnGoSettings.isEnabled = false
            return
        }

        if (satisfied) {
            tvBadge.text = "✅ 已开启"
            tvBadge.setTextColor(Color.parseColor("#2E7D32"))
            tvBadge.background = createBadgeBackground(Color.parseColor("#E8F5E9"))
            btnGoSettings.isEnabled = false
            btnGoSettings.alpha = 0.5f
            if (kind == PermKind.AUTOSTART) {
                btnAutostartConfirm.isEnabled = false
                btnAutostartConfirm.alpha = 0.5f
            }
        } else {
            tvBadge.text = "❌ 未开启"
            tvBadge.setTextColor(Color.parseColor("#C62828"))
            tvBadge.background = createBadgeBackground(Color.parseColor("#FFEBEE"))
            btnGoSettings.isEnabled = true
            btnGoSettings.alpha = 1.0f
            if (kind == PermKind.AUTOSTART) {
                btnAutostartConfirm.isEnabled = true
                btnAutostartConfirm.alpha = 1.0f
            }
        }
    }

    private fun createBadgeBackground(color: Int): GradientDrawable {
        return GradientDrawable().apply {
            setColor(color)
            cornerRadius = 12f
        }
    }

    private fun updateStartButton(satisfiedCount: Int) {
        val total = PermKind.values().size
        val allGreen = satisfiedCount == total

        if (allGreen) {
            btnStartService.text = "全部就绪，启动服务"
            btnStartService.isEnabled = true
            btnStartService.setBackgroundColor(Color.parseColor("#2E7D32"))
            btnStartService.setTextColor(Color.WHITE)
        } else {
            val remaining = total - satisfiedCount
            btnStartService.text = "还差 $remaining 项设置"
            btnStartService.isEnabled = false
            btnStartService.setBackgroundColor(Color.parseColor("#BDBDBD"))
            btnStartService.setTextColor(Color.WHITE)
        }
    }

    // ─────────────────────────────────────────────
    // Service start
    // ─────────────────────────────────────────────

    private fun onStartServiceClicked() {
        if (!PermissionUtils.allSatisfied(this, prefs)) {
            // Not all satisfied — refresh and return
            refreshAllCards()
            return
        }

        // Read config and start service (same shape as MainActivity)
        val cfg = loadConfig()
        if (cfg == null || cfg.server.isEmpty() || cfg.vkey.isEmpty()) {
            Log.e(TAG, "Cannot start service: invalid config")
            finish()
            return
        }

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

        // Return to MainActivity
        finish()
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
}
