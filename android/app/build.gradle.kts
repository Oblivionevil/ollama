plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.android)
}

val appVersionName = providers.gradleProperty("appVersionName").orElse("0.0.0").get()
val appVersionCode = providers.gradleProperty("appVersionCode").orElse("1").map(String::toInt).get()
val androidKeystorePath = providers.gradleProperty("androidKeystorePath")
    .orElse(providers.environmentVariable("ANDROID_KEYSTORE_PATH"))
    .orNull
val androidKeystoreType = providers.gradleProperty("androidKeystoreType")
    .orElse(providers.environmentVariable("ANDROID_KEYSTORE_TYPE"))
    .orNull
val androidKeystorePassword = providers.gradleProperty("androidKeystorePassword")
    .orElse(providers.environmentVariable("ANDROID_KEYSTORE_PASSWORD"))
    .orNull
val androidKeyAlias = providers.gradleProperty("androidKeyAlias")
    .orElse(providers.environmentVariable("ANDROID_KEY_ALIAS"))
    .orNull
val androidKeyPassword = providers.gradleProperty("androidKeyPassword")
    .orElse(providers.environmentVariable("ANDROID_KEY_PASSWORD"))
    .orNull
val hasReleaseSigning = listOf(
    androidKeystorePath,
    androidKeystorePassword,
    androidKeyAlias,
    androidKeyPassword,
).all { !it.isNullOrBlank() }

android {
    namespace = "com.ollama.app"
    compileSdk = 35

    signingConfigs {
        if (hasReleaseSigning) {
            create("release") {
                storeFile = file(androidKeystorePath!!)
                storeType = androidKeystoreType ?: "PKCS12"
                storePassword = androidKeystorePassword
                keyAlias = androidKeyAlias
                keyPassword = androidKeyPassword
                enableV1Signing = true
                enableV2Signing = true
            }
        }
    }

    defaultConfig {
        applicationId = "com.ollama.app"
        minSdk = 26
        targetSdk = 35
        versionCode = appVersionCode
        versionName = appVersionName
    }

    buildTypes {
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            if (hasReleaseSigning) {
                signingConfig = signingConfigs.getByName("release")
            }
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro"
            )
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }
}

dependencies {
    // The gomobile-generated AAR is placed here by the build script
    implementation(fileTree(mapOf("dir" to "libs", "include" to listOf("*.aar"))))

    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.appcompat)
    implementation(libs.material)
    implementation(libs.androidx.webkit)
    implementation(libs.androidx.lifecycle.runtime)
    implementation(libs.androidx.lifecycle.process)
}
