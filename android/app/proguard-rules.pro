# Keep gomobile-generated classes
-keep class ollama.** { *; }
-keep class go.** { *; }

# Keep JavaScript interface methods
-keepclassmembers class com.ollama.app.WebViewBridge {
    @android.webkit.JavascriptInterface <methods>;
}
