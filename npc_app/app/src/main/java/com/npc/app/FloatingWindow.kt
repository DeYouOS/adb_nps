package com.npc.app

import android.annotation.SuppressLint
import android.content.Context
import android.graphics.PixelFormat
import android.graphics.drawable.GradientDrawable
import android.os.Build
import android.view.Gravity
import android.view.MotionEvent
import android.view.View
import android.view.WindowManager
import android.widget.LinearLayout
import android.widget.TextView

class FloatingWindow(private val context: Context) {

    private var windowManager: WindowManager? = null
    private var floatingView: LinearLayout? = null
    private var statusDot: View? = null
    private var statusText: TextView? = null
    private var isShowing = false

    /** Callback when user taps the floating chip */
    var onClickListener: (() -> Unit)? = null

    @SuppressLint("ClickableViewAccessibility")
    fun show() {
        if (isShowing) return

        windowManager = context.getSystemService(Context.WINDOW_SERVICE) as WindowManager

        // Container: small rounded chip
        val container = LinearLayout(context).apply {
            orientation = LinearLayout.HORIZONTAL
            setPadding(28, 14, 28, 14)
            background = GradientDrawable().apply {
                setColor(0xE6000000.toInt()) // semi-transparent black
                cornerRadius = 48f
            }
        }

        // Green/red dot
        statusDot = View(context).apply {
            background = GradientDrawable().apply {
                shape = GradientDrawable.OVAL
                setColor(0xFFFF0000.toInt()) // red = disconnected
            }
        }
        val dotSize = 24
        val dotParams = LinearLayout.LayoutParams(dotSize, dotSize).apply {
            gravity = Gravity.CENTER_VERTICAL
            marginEnd = 12
        }
        container.addView(statusDot, dotParams)

        // Label
        statusText = TextView(context).apply {
            text = "NPC"
            setTextColor(0xFFFFFFFF.toInt())
            textSize = 12f
        }
        container.addView(statusText)

        floatingView = container

        val params = WindowManager.LayoutParams(
            WindowManager.LayoutParams.WRAP_CONTENT,
            WindowManager.LayoutParams.WRAP_CONTENT,
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O)
                WindowManager.LayoutParams.TYPE_APPLICATION_OVERLAY
            else
                @Suppress("DEPRECATION")
                WindowManager.LayoutParams.TYPE_PHONE,
            WindowManager.LayoutParams.FLAG_NOT_FOCUSABLE,
            PixelFormat.TRANSLUCENT
        ).apply {
            gravity = Gravity.TOP or Gravity.END
            x = 16
            y = 200
            // Apply keep-screen-on if persisted
            val prefs = context.getSharedPreferences("npc_prefs", Context.MODE_PRIVATE)
            if (prefs.getBoolean("keep_screen_on", false)) {
                flags = flags or WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON
            }
        }

        // Drag + tap handling
        var initialX = 0
        var initialY = 0
        var initialTouchX = 0f
        var initialTouchY = 0f
        var isDragging = false

        container.setOnTouchListener { _, event ->
            when (event.action) {
                MotionEvent.ACTION_DOWN -> {
                    initialX = params.x
                    initialY = params.y
                    initialTouchX = event.rawX
                    initialTouchY = event.rawY
                    isDragging = false
                    true
                }
                MotionEvent.ACTION_MOVE -> {
                    val dx = event.rawX - initialTouchX
                    val dy = event.rawY - initialTouchY
                    if (dx * dx + dy * dy > 25) isDragging = true
                    params.x = initialX - dx.toInt()
                    params.y = initialY + dy.toInt()
                    try {
                        windowManager?.updateViewLayout(container, params)
                    } catch (_: Exception) {}
                    true
                }
                MotionEvent.ACTION_UP -> {
                    if (!isDragging) onClickListener?.invoke()
                    true
                }
                else -> false
            }
        }

        windowManager?.addView(container, params)
        isShowing = true
    }

    fun hide() {
        if (!isShowing) return
        try {
            windowManager?.removeView(floatingView)
        } catch (_: Exception) {}
        isShowing = false
    }

    fun updateStatus(connected: Boolean, text: String) {
        val color = if (connected) 0xFF00FF00.toInt() else 0xFFFF0000.toInt()
        statusDot?.post {
            (statusDot?.background as? GradientDrawable)?.setColor(color)
        }
        statusText?.post { statusText?.text = text }
    }

    fun setKeepScreenOn(enabled: Boolean) {
        val view = floatingView ?: return
        val wm = windowManager ?: return
        try {
            val params = view.layoutParams as WindowManager.LayoutParams
            if (enabled) {
                params.flags = params.flags or WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON
            } else {
                params.flags = params.flags and WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON.inv()
            }
            wm.updateViewLayout(view, params)
        } catch (_: Exception) {}
    }

    fun isVisible(): Boolean = isShowing
}
