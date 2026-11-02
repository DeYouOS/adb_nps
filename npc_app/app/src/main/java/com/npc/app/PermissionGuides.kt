package com.npc.app

import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.provider.Settings

/**
 * Permission kinds required for the NPC tunnel service.
 */
enum class PermKind {
    OVERLAY,
    NOTIFICATION,
    BATTERY,
    AUTOSTART
}

/**
 * Data class holding all guide information for a single permission on a specific ROM.
 */
data class PermGuide(
    val kind: PermKind,
    val title: String,          // e.g. "悬浮窗权限"
    val purpose: String,        // one short line why it's needed
    val candidates: List<Intent>,
    val menuPath: String,       // ROM-specific menu path
    val steps: List<String>,    // ROM-specific numbered steps
    val detectable: Boolean     // false for AUTOSTART on AOSP-like ROMs
)

/**
 * Provides per-ROM, per-permission guide data.
 *
 * All component names and intent actions are taken verbatim from the research file.
 * Known bugs (HyperOS overlay, OPPO/vivo SecurityException, etc.) are handled here.
 */
object PermissionGuides {

    /**
     * Get the [PermGuide] for a given [PermKind] on the current device.
     */
    fun guideFor(kind: PermKind, ctx: Context): PermGuide {
        val pkg = ctx.packageName
        return when (kind) {
            PermKind.OVERLAY -> overlayGuide(pkg)
            PermKind.NOTIFICATION -> notificationGuide(pkg)
            PermKind.BATTERY -> batteryGuide(pkg)
            PermKind.AUTOSTART -> autostartGuide(pkg)
        }
    }

    // ─────────────────────────────────────────────
    // OVERLAY
    // ─────────────────────────────────────────────

    private fun overlayGuide(pkg: String): PermGuide {
        val rom = RomDetector.family
        val candidates = mutableListOf<Intent>()

        when (rom) {
            // MIUI and HyperOS: try MIUI PermissionsEditorActivity first
            // (real-device test on Xiaomi 11 HyperOS V816 confirms the editor
            //  lands on an app-scoped page; user then taps 其他权限 → 显示悬浮窗)
            RomFamily.MIUI, RomFamily.HYPEROS -> {
                candidates.add(Intent("miui.intent.action.APP_PERM_EDITOR").apply {
                    component = ComponentName(
                        "com.miui.securitycenter",
                        "com.miui.permcenter.permissions.PermissionsEditorActivity"
                    )
                    putExtra("extra_pkgname", pkg)
                })
                candidates.add(Intent("miui.intent.action.APP_PERM_EDITOR").apply {
                    component = ComponentName(
                        "com.miui.securitycenter",
                        "com.miui.permcenter.permissions.AppPermissionsEditorActivity"
                    )
                    putExtra("extra_pkgname", pkg)
                })
            }
            // Huawei EMUI ≤3.0
            RomFamily.EMUI -> {
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.huawei.systemmanager",
                        "com.huawei.notificationmanager.ui.NotificationManagmentActivity"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.huawei.systemmanager",
                        "com.huawei.systemmanager.addviewmonitor.AddViewMonitorActivity"
                    )
                })
            }
            // Huawei HarmonyOS
            RomFamily.HARMONY -> {
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.huawei.systemmanager",
                        "com.huawei.systemmanager.addviewmonitor.AddViewMonitorActivity"
                    )
                })
            }
            // Honor MagicOS
            RomFamily.MAGICOS -> {
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.hihonor.systemmanager",
                        "com.hihonor.systemmanager.addviewmonitor.AddViewMonitorActivity"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.huawei.systemmanager",
                        "com.huawei.systemmanager.addviewmonitor.AddViewMonitorActivity"
                    )
                })
            }
            // ColorOS: AOSP is sufficient; optional backup
            RomFamily.COLOROS -> {
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.coloros.safecenter",
                        "com.coloros.privacypermissionsentry.PermissionTopActivity"
                    )
                })
            }
            // OriginOS / Funtouch: AOSP is sufficient; optional backup
            RomFamily.ORIGINOS, RomFamily.FUNTOUCH -> {
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.vivo.permissionmanager",
                        "com.vivo.permissionmanager.activity.SoftPermissionDetailActivity"
                    )
                })
            }
            // Samsung / AOSP: no special candidates needed
            RomFamily.ONEUI, RomFamily.AOSP -> { }
        }

        // Common AOSP overlay intents (all ROMs)
        candidates.add(Intent(Settings.ACTION_MANAGE_OVERLAY_PERMISSION).apply {
            data = Uri.parse("package:$pkg")
        })
        candidates.add(Intent(Settings.ACTION_MANAGE_OVERLAY_PERMISSION))

        // Final fallback: app details
        candidates.add(appDetailsIntent(pkg))

        return PermGuide(
            kind = PermKind.OVERLAY,
            title = "悬浮窗权限",
            purpose = "悬浮窗用于显示连接状态浮标，方便随时查看",
            candidates = candidates,
            menuPath = overlayMenuPath(),
            steps = overlaySteps(),
            detectable = true
        )
    }

    private fun overlayMenuPath(): String = when (RomDetector.family) {
        RomFamily.MIUI, RomFamily.HYPEROS ->
            "设置 → 应用设置 → 应用管理 → 本应用 → 权限管理 → 其他权限 → 显示悬浮窗 → 允许"
        RomFamily.COLOROS ->
            "设置 → 权限与隐私 → 悬浮窗 → 找到本应用 → 开启"
        RomFamily.ORIGINOS ->
            "设置 → 应用与权限 → 权限管理 → 悬浮窗 → 找到本应用 → 开启"
        RomFamily.FUNTOUCH ->
            "i管家 → 权限管理 → 悬浮窗 → 找到本应用 → 开启"
        RomFamily.EMUI, RomFamily.HARMONY ->
            "设置 → 应用和服务 → 权限管理 → 悬浮窗 → 找到本应用 → 允许"
        RomFamily.MAGICOS ->
            "设置 → 应用 → 权限管理 → 悬浮窗 → 找到本应用 → 允许"
        RomFamily.ONEUI ->
            "设置 → 应用 → 特殊访问 → 显示在其他应用的上层 → 找到本应用 → 允许"
        RomFamily.AOSP ->
            "设置 → 应用 → 特殊应用权限 → 显示在其他应用上层"
    }

    private fun overlaySteps(): List<String> = when (RomDetector.family) {
        RomFamily.MIUI, RomFamily.HYPEROS -> listOf(
            "在弹出的页面里点「权限管理」",
            "点击「其他权限」",
            "找到「显示悬浮窗」",
            "选择「允许」"
        )
        RomFamily.COLOROS -> listOf(
            "打开手机「设置」",
            "进入「权限与隐私」",
            "点击「悬浮窗」",
            "找到本应用，开启开关"
        )
        RomFamily.ORIGINOS -> listOf(
            "打开手机「设置」",
            "进入「应用与权限」→「权限管理」",
            "点击「悬浮窗」",
            "找到本应用，开启开关"
        )
        RomFamily.FUNTOUCH -> listOf(
            "打开「i管家」",
            "进入「权限管理」",
            "点击「悬浮窗」",
            "找到本应用，开启开关"
        )
        RomFamily.EMUI, RomFamily.HARMONY -> listOf(
            "打开手机「设置」",
            "进入「应用和服务」→「权限管理」",
            "点击「悬浮窗」",
            "找到本应用，选择「允许」"
        )
        RomFamily.MAGICOS -> listOf(
            "打开手机「设置」",
            "进入「应用」→「权限管理」",
            "点击「悬浮窗」",
            "找到本应用，选择「允许」"
        )
        RomFamily.ONEUI -> listOf(
            "打开手机「设置」",
            "进入「应用」→「特殊访问」",
            "点击「显示在其他应用的上层」",
            "找到本应用，开启开关"
        )
        RomFamily.AOSP -> listOf(
            "打开手机「设置」",
            "进入「应用」→「特殊应用权限」",
            "点击「显示在其他应用上层」",
            "找到本应用，开启开关"
        )
    }

    // ─────────────────────────────────────────────
    // NOTIFICATION
    // ─────────────────────────────────────────────

    private fun notificationGuide(pkg: String): PermGuide {
        val candidates = mutableListOf<Intent>()

        // System notification settings
        candidates.add(Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS).apply {
            putExtra(Settings.EXTRA_APP_PACKAGE, pkg)
        })
        // Huawei-specific
        if (RomDetector.family == RomFamily.EMUI || RomDetector.family == RomFamily.HARMONY) {
            candidates.add(Intent().apply {
                component = ComponentName(
                    "com.huawei.notificationmanager",
                    "com.huawei.notificationmanager.ui.NotificationSettingsActivity"
                )
            })
        }
        // Fallback
        candidates.add(appDetailsIntent(pkg))

        return PermGuide(
            kind = PermKind.NOTIFICATION,
            title = "通知权限",
            purpose = "通知用于显示服务运行状态和连接提示",
            candidates = candidates,
            menuPath = "设置 → 通知 → 找到本应用 → 允许通知",
            steps = listOf(
                "打开手机「设置」",
                "进入「通知」或「通知管理」",
                "找到本应用",
                "开启「允许通知」"
            ),
            detectable = true
        )
    }

    // ─────────────────────────────────────────────
    // BATTERY
    // ─────────────────────────────────────────────

    private fun batteryGuide(pkg: String): PermGuide {
        val rom = RomDetector.family
        val candidates = mutableListOf<Intent>()

        // All ROMs: try ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS first
        candidates.add(Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS).apply {
            data = Uri.parse("package:$pkg")
        })

        // Android 12+: advanced power usage detail
        if (android.os.Build.VERSION.SDK_INT >= 31) {
            try {
                candidates.add(Intent("android.settings.VIEW_ADVANCED_POWER_USAGE_DETAIL").apply {
                    data = Uri.parse("package:$pkg")
                })
            } catch (_: Exception) {}
        }

        // ROM-specific candidates
        when (rom) {
            RomFamily.COLOROS -> {
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.coloros.safecenter",
                        "com.coloros.safecenter.appfrozen.activity.AppFrozenSettingsActivity"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.coloros.oppoguardelf",
                        "com.coloros.powermanager.fuelgaue.PowerConsumptionActivity"
                    )
                })
            }
            RomFamily.ORIGINOS, RomFamily.FUNTOUCH -> {
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.vivo.abe",
                        "com.vivo.applicationbehaviorengine.ui.ExcessivePowerManagerActivity"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.iqoo.powersaving",
                        "com.iqoo.powersaving.PowerSavingManagerActivity"
                    )
                })
            }
            RomFamily.EMUI, RomFamily.HARMONY -> {
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.huawei.systemmanager",
                        "com.huawei.systemmanager.optimize.process.ProtectActivity"
                    )
                })
            }
            RomFamily.MAGICOS -> {
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.hihonor.systemmanager",
                        "com.hihonor.systemmanager.optimize.process.ProtectActivity"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.huawei.systemmanager",
                        "com.huawei.systemmanager.optimize.process.ProtectActivity"
                    )
                })
            }
            RomFamily.ONEUI -> {
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.samsung.android.lool",
                        "com.samsung.android.sm.battery.ui.BatteryActivity"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.samsung.android.lool",
                        "com.samsung.android.sm.ui.battery.BatteryActivity"
                    )
                })
            }
            RomFamily.MIUI, RomFamily.HYPEROS, RomFamily.AOSP -> {
                // No ROM-specific battery candidates beyond the universal ones
            }
        }

        // Common fallbacks
        candidates.add(Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS))
        candidates.add(appDetailsIntent(pkg))

        return PermGuide(
            kind = PermKind.BATTERY,
            title = "电池优化白名单",
            purpose = "防止系统在后台杀死本应用，确保隧道持续在线",
            candidates = candidates,
            menuPath = batteryMenuPath(),
            steps = batterySteps(),
            detectable = true
        )
    }

    private fun batteryMenuPath(): String = when (RomDetector.family) {
        RomFamily.MIUI, RomFamily.HYPEROS ->
            "设置 → 应用设置 → 应用管理 → 本应用 → 省电策略 → 无限制"
        RomFamily.COLOROS ->
            "设置 → 电池 → 耗电管理 → 本应用 → 不限制（并关闭「应用速冻」）"
        RomFamily.ORIGINOS ->
            "设置 → 电池 → 后台耗电管理 → 本应用 → 允许后台高耗电"
        RomFamily.FUNTOUCH ->
            "设置 → 电池 → 后台耗电管理 → 本应用 → 允许后台高耗电"
        RomFamily.EMUI, RomFamily.HARMONY ->
            "设置 → 电池 → 启动管理 → 本应用 → 手动管理 → 全部打开"
        RomFamily.MAGICOS ->
            "设置 → 电池 → 启动管理 → 本应用 → 手动管理 → 全部允许"
        RomFamily.ONEUI ->
            "设置 → 电池和设备维护 → 电池 → 后台使用限制 → 确保本应用不在「深度休眠应用」里"
        RomFamily.AOSP ->
            "设置 → 应用 → 本应用 → 电池 → 无限制"
    }

    private fun batterySteps(): List<String> = when (RomDetector.family) {
        RomFamily.MIUI, RomFamily.HYPEROS -> listOf(
            "打开手机「设置」",
            "进入「应用设置」→「应用管理」",
            "找到并点击本应用",
            "点击「省电策略」",
            "选择「无限制」"
        )
        RomFamily.COLOROS -> listOf(
            "打开手机「设置」",
            "进入「电池」→「耗电管理」",
            "找到本应用",
            "选择「不限制」",
            "同时关闭「应用速冻」（如有）"
        )
        RomFamily.ORIGINOS, RomFamily.FUNTOUCH -> listOf(
            "打开手机「设置」",
            "进入「电池」→「后台耗电管理」",
            "找到本应用",
            "选择「允许后台高耗电」"
        )
        RomFamily.EMUI, RomFamily.HARMONY -> listOf(
            "打开手机「设置」",
            "进入「电池」→「启动管理」",
            "找到本应用",
            "关闭「自动管理」",
            "手动管理中三个开关全部打开"
        )
        RomFamily.MAGICOS -> listOf(
            "打开手机「设置」",
            "进入「电池」→「启动管理」",
            "找到本应用",
            "关闭「自动管理」",
            "手动管理中全部允许"
        )
        RomFamily.ONEUI -> listOf(
            "打开手机「设置」",
            "进入「电池和设备维护」→「电池」",
            "进入「后台使用限制」",
            "确保本应用不在「深度休眠应用」列表中"
        )
        RomFamily.AOSP -> listOf(
            "打开手机「设置」",
            "进入「应用」→ 本应用",
            "点击「电池」",
            "选择「无限制」"
        )
    }

    // ─────────────────────────────────────────────
    // AUTOSTART (undetectable → manual confirmation)
    // ─────────────────────────────────────────────

    private fun autostartGuide(pkg: String): PermGuide {
        val rom = RomDetector.family

        // AOSP-like ROMs have no autostart concept
        if (rom == RomFamily.AOSP) {
            return PermGuide(
                kind = PermKind.AUTOSTART,
                title = "自启动 / 后台运行",
                purpose = "允许应用在后台自动运行，保持隧道持续连接",
                candidates = emptyList(),
                menuPath = "无需设置",
                steps = listOf("当前系统无需额外设置自启动权限"),
                detectable = false
            )
        }

        val candidates = mutableListOf<Intent>()

        when (rom) {
            RomFamily.MIUI, RomFamily.HYPEROS -> {
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.miui.securitycenter",
                        "com.miui.permcenter.autostart.AutoStartManagementActivity"
                    )
                })
                candidates.add(Intent("miui.intent.action.OP_AUTO_START").apply {
                    addCategory(Intent.CATEGORY_DEFAULT)
                })
                candidates.add(Intent("miui.intent.action.POWER_HIDE_MODE_APP_LIST").apply {
                    addCategory(Intent.CATEGORY_DEFAULT)
                    putExtra("package_name", pkg)
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.miui.securitycenter",
                        "com.miui.powercenter.PowerSettings"
                    )
                })
            }
            RomFamily.COLOROS -> {
                // OPPO may throw SecurityException — caught by launchFirstResolvable
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.coloros.safecenter",
                        "com.coloros.safecenter.startupapp.StartupAppListActivity"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.coloros.safecenter",
                        "com.coloros.safecenter.permission.startupapp.StartupAppListActivity"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.coloros.safecenter",
                        "com.coloros.privacypermissionsentry.PermissionTopActivity"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.oppo.safe",
                        "com.oppo.safe.permission.startup.StartupAppListActivity"
                    )
                })
            }
            RomFamily.ORIGINOS, RomFamily.FUNTOUCH -> {
                // vivo may throw SecurityException — caught by launchFirstResolvable
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.vivo.permissionmanager",
                        "com.vivo.permissionmanager.activity.BgStartUpManagerActivity"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.iqoo.secure",
                        "com.iqoo.secure.ui.phoneoptimize.AddWhiteListActivity"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.iqoo.secure",
                        "com.iqoo.secure.phoneoptimize.BgStartUpManager"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.vivo.permissionmanager",
                        "com.vivo.permissionmanager.activity.PurviewTabActivity"
                    )
                })
            }
            RomFamily.EMUI, RomFamily.HARMONY -> {
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.huawei.systemmanager",
                        "com.huawei.systemmanager.startupmgr.ui.StartupNormalAppListActivity"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.huawei.systemmanager",
                        "com.huawei.systemmanager.appcontrol.activity.StartupAppControlActivity"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.huawei.systemmanager",
                        "com.huawei.systemmanager.optimize.process.ProtectActivity"
                    )
                })
            }
            RomFamily.MAGICOS -> {
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.hihonor.systemmanager",
                        "com.hihonor.systemmanager.startupmgr.ui.StartupNormalAppListActivity"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.hihonor.systemmanager",
                        "com.hihonor.systemmanager.appcontrol.activity.StartupAppControlActivity"
                    )
                })
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.hihonor.systemmanager",
                        "com.hihonor.systemmanager.optimize.process.ProtectActivity"
                    )
                })
                // Fallback to Huawei
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.huawei.systemmanager",
                        "com.huawei.systemmanager.optimize.process.ProtectActivity"
                    )
                })
            }
            RomFamily.ONEUI -> {
                // Samsung: no dedicated autostart page; show "Sleeping apps"
                candidates.add(Intent().apply {
                    component = ComponentName(
                        "com.samsung.android.lool",
                        "com.samsung.android.lool.activity.applist.AppListActivity"
                    )
                })
            }
            RomFamily.AOSP -> {
                // Handled above (detectable=false)
            }
        }

        // Final fallback
        candidates.add(appDetailsIntent(pkg))

        return PermGuide(
            kind = PermKind.AUTOSTART,
            title = "自启动 / 后台运行",
            purpose = "允许应用在后台自动运行，保持隧道持续连接",
            candidates = candidates,
            menuPath = autostartMenuPath(),
            steps = autostartSteps(),
            detectable = true
        )
    }

    private fun autostartMenuPath(): String = when (RomDetector.family) {
        RomFamily.MIUI, RomFamily.HYPEROS ->
            "设置 → 应用设置 → 应用管理 → 本应用 → 自启动 → 允许；省电策略 → 无限制"
        RomFamily.COLOROS ->
            "设置 → 权限与隐私 → 权限管理 → 自启动 → 允许"
        RomFamily.ORIGINOS ->
            "设置 → 应用与权限 → 权限管理 → 自启动 → 允许"
        RomFamily.FUNTOUCH ->
            "设置 → 应用与权限 → 权限管理 → 自启动 → 允许"
        RomFamily.EMUI, RomFamily.HARMONY ->
            "设置 → 应用和服务 → 应用启动管理 → 本应用 → 关闭「自动管理」→ 三个开关全开"
        RomFamily.MAGICOS ->
            "设置 → 应用 → 应用启动管理 → 本应用 → 手动管理 → 全部允许"
        RomFamily.ONEUI ->
            "设置 → 电池和设备维护 → 电池 → 后台使用限制 → 把本应用从「深度休眠应用」移除"
        RomFamily.AOSP ->
            "无需设置"
    }

    private fun autostartSteps(): List<String> = when (RomDetector.family) {
        RomFamily.MIUI, RomFamily.HYPEROS -> listOf(
            "打开手机「设置」",
            "进入「应用设置」→「应用管理」",
            "找到并点击本应用",
            "进入「自启动」页面（应用列表）",
            "在「禁止自启动」列表中找到本应用，切换为允许",
            "返回上一页，进入「省电策略」",
            "选择「无限制」"
        )
        RomFamily.COLOROS -> listOf(
            "打开手机「设置」",
            "进入「权限与隐私」→「权限管理」",
            "点击「自启动」",
            "找到本应用，开启开关"
        )
        RomFamily.ORIGINOS, RomFamily.FUNTOUCH -> listOf(
            "打开手机「设置」",
            "进入「应用与权限」→「权限管理」",
            "点击「自启动」",
            "找到本应用，开启开关"
        )
        RomFamily.EMUI, RomFamily.HARMONY -> listOf(
            "打开手机「设置」",
            "进入「应用和服务」→「应用启动管理」",
            "找到本应用",
            "关闭「自动管理」",
            "手动管理中三个开关（自启动、关联启动、后台活动）全部打开"
        )
        RomFamily.MAGICOS -> listOf(
            "打开手机「设置」",
            "进入「应用」→「应用启动管理」",
            "找到本应用",
            "关闭「自动管理」",
            "手动管理中全部允许"
        )
        RomFamily.ONEUI -> listOf(
            "打开手机「设置」",
            "进入「电池和设备维护」→「电池」",
            "进入「后台使用限制」",
            "把本应用从「深度休眠应用」列表中移除"
        )
        RomFamily.AOSP -> listOf(
            "当前系统无需额外设置自启动权限"
        )
    }

    // ─────────────────────────────────────────────
    // Utility
    // ─────────────────────────────────────────────

    private fun appDetailsIntent(pkg: String): Intent {
        return Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS).apply {
            data = Uri.parse("package:$pkg")
            addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        }
    }
}
