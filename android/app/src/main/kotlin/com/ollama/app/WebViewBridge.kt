package com.ollama.app

import android.content.Intent
import android.net.Uri
import android.util.Log
import android.webkit.JavascriptInterface
import android.webkit.WebView

/**
 * JavaScript interface exposed to the WebView as `AndroidBridge`.
 * Provides native Android functionality to the React SPA.
 */
class WebViewBridge(
    private val activity: MainActivity,
    private val webView: WebView
) {
    companion object {
        private const val TAG = "WebViewBridge"
    }

    @JavascriptInterface
    fun ready() {
        Log.d(TAG, "WebView signalled ready")
    }

    @JavascriptInterface
    fun openExternal(url: String) {
        if (url.isBlank()) return
        try {
            val intent = Intent(Intent.ACTION_VIEW, Uri.parse(url))
            activity.startActivity(intent)
        } catch (e: Exception) {
            Log.e(TAG, "Failed to open external URL: $url", e)
        }
    }

    @JavascriptInterface
    fun selectFiles() {
        activity.runOnUiThread {
            activity.launchFilePicker()
        }
    }

    @JavascriptInterface
    fun selectModelsDirectory() {
        activity.runOnUiThread {
            activity.launchDirectoryPicker()
        }
    }
}
