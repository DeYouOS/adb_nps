package com.npc.app

import android.Manifest
import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.content.SharedPreferences
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.PowerManager
import android.provider.Settings
import android.util.Log
import androidx.core.app.ActivityCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat

/**
 * Permission checking and launching utilities.
 *
 * Implements the detection rules from the research file §1:
 * - OVERLAY: Settings.canDrawOverlays
 * - NOTIFICATION: A13+ permission AND NotificationManagerCompat
 * - BATTERY: PowerManager.isIgnoringBatteryOptimizations
 * - AUTOSTART: undetectable → SharedPreferences manual confirmation
 */
object PermissionUtils {

    private const val TAG = "PermissionUtils"
    private const val PREFS_NAME = "npc_prefs"
    private const val KEY_AUTOSTART_CONFIRMED = "perm_autostart_confirmed"

    const val REQUEST_NOTIFICATION = 2001

    // ─────────────────────────────────────────────
    // Permission checking
    // ─────────────────────────────────────────────

    /**
     * Check if a specific permission kind is currently satisfied.
     *
     * @param kind The permission kind to check
     * @param ctx Context
     * @param prefs SharedPreferences (for AUTOSTART manual confirmation)
     * @return true if the permission is granted/satisfied
     */
    fun check(kind: PermKind, ctx: Context, prefs: SharedPreferences): Boolean {
        return when (kind) {
            PermKind.OVERLAY -> checkOverlay(ctx)
            PermKind.NOTIFICATION -> checkNotification(ctx)
            PermKind.BATTERY -> checkBattery(ctx)
            PermKind.AUTOSTART -> checkAutostart(ctx, prefs)
        }
    }

    /**
     * Check if ALL required permissions are satisfied.
     */
    fun allSatisfied(ctx: Context, prefs: SharedPreferences): Boolean {
        return PermKind.values().all { check(it, ctx, prefs) }
    }

    /**
     * Count how many permissions are currently satisfied.
     */
    fun countSatisfied(ctx: Context, prefs: SharedPreferences): Int {
        return PermKind.values().count { check(it, ctx, prefs) }
    }

    private fun checkOverlay(ctx: Context): Boolean {
        return Settings.canDrawOverlays(ctx)
    }

    private fun checkNotification(ctx: Context): Boolean {
        // Android 13+: need both runtime permission AND notification enabled
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            val permGranted = ContextCompat.checkSelfPermission(
                ctx, Manifest.permission.POST_NOTIFICATIONS
            ) == PackageManager.PERMISSION_GRANTED
            if (!permGranted) return false
        }
        // All versions: check if notifications are enabled in system settings
        return NotificationManagerCompat.from(ctx).areNotificationsEnabled()
    }

    private fun checkBattery(ctx: Context): Boolean {
        return try {
            val pm = ctx.getSystemService(Context.POWER_SERVICE) as PowerManager
            pm.isIgnoringBatteryOptimizations(ctx.packageName)
        } catch (e: Exception) {
            Log.w(TAG, "checkBattery failed: ${e.message}")
            false
        }
    }

    private fun checkAutostart(@Suppress("UNUSED_PARAMETER") ctx: Context, prefs: SharedPreferences): Boolean {
        // AOSP-like ROMs have no autostart concept → auto-satisfied
        if (RomDetector.family == RomFamily.AOSP) {
            return true
        }
        return prefs.getBoolean(KEY_AUTOSTART_CONFIRMED, false)
    }

    // ─────────────────────────────────────────────
    // Intent launching
    // ─────────────────────────────────────────────

    /**
     * Try to launch the first resolvable intent from a list of candidates.
     *
     * Per research §5: try each with resolveActivity + addFlags(FLAG_ACTIVITY_NEW_TASK),
     * catch ActivityNotFoundException and SecurityException (OPPO/vivo common),
     * finally fall back to ACTION_APPLICATION_DETAILS_SETTINGS.
     *
     * @return true if something was successfully launched
     */
    fun launchFirstResolvable(activity: Activity, candidates: List<Intent>): Boolean {
        for (intent in candidates) {
            try {
                intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                if (intent.resolveActivity(activity.packageManager) != null) {
                    activity.startActivity(intent)
                    return true
                }
            } catch (_: ActivityNotFoundException) {
                // Component not found, try next
            } catch (_: SecurityException) {
                // OPPO/vivo autostart may throw this — try next
            } catch (e: Exception) {
                Log.w(TAG, "launchFirstResolvable unexpected error: ${e.message}")
            }
        }
        // Final fallback: app details page
        return try {
            activity.startActivity(
                Intent(
                    Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
                    Uri.parse("package:${activity.packageName}")
                ).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
            )
            true
        } catch (_: Exception) {
            Log.e(TAG, "Even ACTION_APPLICATION_DETAILS_SETTINGS failed")
            false
        }
    }

    // ─────────────────────────────────────────────
    // Notification runtime permission request
    // ─────────────────────────────────────────────

    /**
     * Request POST_NOTIFICATIONS runtime permission on Android 13+.
     * The guide should trigger the real system dialog first for NOTIFICATION,
     * and only fall back to settings if denied.
     *
     * @return true if the permission was already granted, false if a request was launched
     */
    fun requestNotificationPermissionIfNeeded(activity: Activity): Boolean {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            if (ContextCompat.checkSelfPermission(
                    activity, Manifest.permission.POST_NOTIFICATIONS
                ) != PackageManager.PERMISSION_GRANTED
            ) {
                ActivityCompat.requestPermissions(
                    activity,
                    arrayOf(Manifest.permission.POST_NOTIFICATIONS),
                    REQUEST_NOTIFICATION
                )
                return false
            }
        }
        return true
    }

    // ─────────────────────────────────────────────
    // Autostart confirmation
    // ─────────────────────────────────────────────

    /**
     * Mark autostart as confirmed by the user.
     */
    fun confirmAutostart(prefs: SharedPreferences) {
        prefs.edit().putBoolean(KEY_AUTOSTART_CONFIRMED, true).apply()
    }

    /**
     * Reset autostart confirmation (for re-check).
     */
    fun resetAutostart(prefs: SharedPreferences) {
        prefs.edit().putBoolean(KEY_AUTOSTART_CONFIRMED, false).apply()
    }
}
