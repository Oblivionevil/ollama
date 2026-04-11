package com.ollama.app

import android.annotation.SuppressLint
import android.content.Intent
import android.net.Uri
import android.os.Bundle
import android.util.Base64
import android.util.Log
import android.view.View
import android.webkit.ValueCallback
import android.webkit.WebChromeClient
import android.webkit.WebResourceRequest
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.ProgressBar
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import androidx.core.view.WindowCompat
import ollama.Ollama
import org.json.JSONArray
import org.json.JSONObject
import java.io.InputStream

class MainActivity : AppCompatActivity() {

    companion object {
        private const val TAG = "OllamaActivity"
    }

    private lateinit var webView: WebView
    private lateinit var loading: ProgressBar

    private var serverPort: Int = 0
    private var serverToken: String = ""

    private var filePickerCallback: ValueCallback<Array<Uri>>? = null

    private val filePickerLauncher = registerForActivityResult(
        ActivityResultContracts.OpenMultipleDocuments()
    ) { uris ->
        if (uris.isNullOrEmpty()) {
            filePickerCallback?.onReceiveValue(null)
            filePickerCallback = null
            webView.evaluateJavascript(
                "window.__selectFilesCallback && window.__selectFilesCallback(null)", null
            )
            return@registerForActivityResult
        }
        handleSelectedFiles(uris)
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        WindowCompat.setDecorFitsSystemWindows(window, false)
        setContentView(R.layout.activity_main)

        webView = findViewById(R.id.webview)
        loading = findViewById(R.id.loading)
        webView.visibility = View.INVISIBLE

        startServer()
        setupWebView()
        loadApp()
    }

    private fun startServer() {
        try {
            val dataDir = filesDir.absolutePath
            val result = Ollama.start(dataDir)
            serverPort = result.port.toInt()
            serverToken = result.token
            ContextCompat.startForegroundService(this, Intent(this, OllamaService::class.java))
            Log.i(TAG, "Server started on port $serverPort")
        } catch (e: Exception) {
            Log.e(TAG, "Failed to start server", e)
        }
    }

    @SuppressLint("SetJavaScriptEnabled")
    private fun setupWebView() {
        webView.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true
            databaseEnabled = true
            allowFileAccess = false
            allowContentAccess = false
            mixedContentMode = WebSettings.MIXED_CONTENT_NEVER_ALLOW
            cacheMode = WebSettings.LOAD_DEFAULT
            setSupportZoom(true)
            builtInZoomControls = true
            displayZoomControls = false
            textZoom = 100
        }

        // JavaScript bridge
        webView.addJavascriptInterface(
            WebViewBridge(this, webView), "AndroidBridge"
        )

        webView.webViewClient = object : WebViewClient() {
            override fun shouldOverrideUrlLoading(
                view: WebView, request: WebResourceRequest
            ): Boolean {
                val url = request.url.toString()
                // Keep localhost navigation in the webview
                if (url.startsWith("http://127.0.0.1:$serverPort")) {
                    return false
                }
                // Open external URLs in the system browser
                startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(url)))
                return true
            }

            override fun onPageFinished(view: WebView, url: String?) {
                super.onPageFinished(view, url)
                loading.visibility = View.GONE
                webView.visibility = View.VISIBLE

                // Inject bridge adapter: map window.* desktop APIs to AndroidBridge
                view.evaluateJavascript(BRIDGE_INIT_JS, null)
            }
        }

        webView.webChromeClient = object : WebChromeClient() {
            override fun onShowFileChooser(
                webView: WebView,
                filePathCallback: ValueCallback<Array<Uri>>,
                fileChooserParams: FileChooserParams
            ): Boolean {
                filePickerCallback?.onReceiveValue(null)
                filePickerCallback = filePathCallback
                filePickerLauncher.launch(arrayOf("*/*"))
                return true
            }
        }
    }

    private fun loadApp() {
        if (serverPort == 0) {
            Log.e(TAG, "Server not started, cannot load app")
            return
        }
        webView.loadUrl("http://127.0.0.1:$serverPort/?token=$serverToken")
    }

    private fun handleSelectedFiles(uris: List<Uri>) {
        Thread {
            try {
                val filesArray = JSONArray()
                for (uri in uris) {
                    val filename = getFileName(uri) ?: "unknown"
                    val data = readFileAsDataURL(uri, filename)
                    if (data != null) {
                        val fileObj = JSONObject().apply {
                            put("filename", filename)
                            put("path", uri.toString())
                            put("dataURL", data)
                        }
                        filesArray.put(fileObj)
                    }
                }
                val js = "window.__selectFilesCallback && window.__selectFilesCallback($filesArray)"
                runOnUiThread {
                    filePickerCallback?.onReceiveValue(uris.toTypedArray())
                    filePickerCallback = null
                    webView.evaluateJavascript(js, null)
                }
            } catch (e: Exception) {
                Log.e(TAG, "Error processing selected files", e)
                runOnUiThread {
                    filePickerCallback?.onReceiveValue(null)
                    filePickerCallback = null
                    webView.evaluateJavascript(
                        "window.__selectFilesCallback && window.__selectFilesCallback(null)", null
                    )
                }
            }
        }.start()
    }

    private fun getFileName(uri: Uri): String? {
        var name: String? = null
        contentResolver.query(uri, null, null, null, null)?.use { cursor ->
            val index = cursor.getColumnIndex(android.provider.OpenableColumns.DISPLAY_NAME)
            if (index >= 0 && cursor.moveToFirst()) {
                name = cursor.getString(index)
            }
        }
        return name
    }

    private fun readFileAsDataURL(uri: Uri, filename: String): String? {
        return try {
            val stream: InputStream = contentResolver.openInputStream(uri) ?: return null
            val bytes = stream.readBytes()
            stream.close()
            val base64 = Base64.encodeToString(bytes, Base64.NO_WRAP)
            val mime = contentResolver.getType(uri) ?: guessMimeType(filename)
            "data:$mime;base64,$base64"
        } catch (e: Exception) {
            Log.e(TAG, "Error reading file $filename", e)
            null
        }
    }

    private fun guessMimeType(filename: String): String {
        val ext = filename.substringAfterLast('.', "").lowercase()
        return when (ext) {
            "pdf" -> "application/pdf"
            "docx" -> "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
            "xlsx" -> "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
            "pptx" -> "application/vnd.openxmlformats-officedocument.presentationml.presentation"
            "png" -> "image/png"
            "jpg", "jpeg" -> "image/jpeg"
            "gif" -> "image/gif"
            "webp" -> "image/webp"
            "txt" -> "text/plain"
            "html", "htm" -> "text/html"
            "json" -> "application/json"
            "csv" -> "text/csv"
            "xml" -> "application/xml"
            else -> "application/octet-stream"
        }
    }

    fun launchFilePicker() {
        filePickerLauncher.launch(arrayOf("*/*"))
    }

    fun launchDirectoryPicker() {
        webView.evaluateJavascript(
            "window.__selectModelsDirectoryCallback && window.__selectModelsDirectoryCallback(null)", null
        )
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        // Handle deep link (e.g. ollama://auth/callback)
        intent.data?.let { uri ->
            val path = uri.path ?: "/"
            webView.loadUrl("http://127.0.0.1:$serverPort$path?token=$serverToken")
        }
    }

    override fun onDestroy() {
        super.onDestroy()
        stopService(Intent(this, OllamaService::class.java))
        Ollama.stop()
    }

    @Deprecated("Use OnBackPressedCallback")
    override fun onBackPressed() {
        if (webView.canGoBack()) {
            webView.goBack()
        } else {
            @Suppress("DEPRECATION")
            super.onBackPressed()
        }
    }
}

/**
 * JavaScript injected after each page load to bridge the desktop webview API
 * (window.openExternal, window.webview.selectFile, etc.) to the Android
 * JavascriptInterface (AndroidBridge).
 */
private const val BRIDGE_INIT_JS = """
(function() {
    // Open external URLs via system browser
    window.openExternal = function(url) {
        AndroidBridge.openExternal(url);
    };

    // Web search flag
    window.OLLAMA_WEBSEARCH = true;

    // No-op window drag/doubleClick (desktop-only)
    window.drag = function() {};
    window.doubleClick = function() {};

    // Context menu — returns immediately (Android has no native context menu API from JS)
    window.menu = function(items) {
        return Promise.resolve(null);
    };

    // File selection API (mirrors desktop webview bridge)
    window.webview = {
        selectFile: function() {
            return new Promise(function(resolve) {
                window.__selectFilesCallback = function(data) {
                    window.__selectFilesCallback = null;
                    resolve(data && data.length > 0 ? data[0] : null);
                };
                AndroidBridge.selectFiles();
            });
        },
        selectMultipleFiles: function() {
            return new Promise(function(resolve) {
                window.__selectFilesCallback = function(data) {
                    window.__selectFilesCallback = null;
                    resolve(data);
                };
                AndroidBridge.selectFiles();
            });
        },
        selectModelsDirectory: function() {
            return new Promise(function(resolve) {
                window.__selectModelsDirectoryCallback = function(data) {
                    window.__selectModelsDirectoryCallback = null;
                    resolve(data);
                };
                AndroidBridge.selectModelsDirectory();
            });
        },
        selectWorkingDirectory: function() {
            return new Promise(function(resolve) {
                window.__selectModelsDirectoryCallback = function(data) {
                    window.__selectModelsDirectoryCallback = null;
                    resolve(data);
                };
                AndroidBridge.selectModelsDirectory();
            });
        }
    };

    // Signal ready
    if (typeof AndroidBridge !== 'undefined') {
        AndroidBridge.ready();
    }
})();
"""
