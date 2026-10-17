// SPDX-License-Identifier: AGPL-3.0-or-later

import java.io.FileInputStream
import java.util.Properties

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.android)
    alias(libs.plugins.kotlin.serialization)
    alias(libs.plugins.kotlin.compose)
}

// --- Version, derived from a git tag rather than a build-time counter. -------
//
// F-Droid's auto-update reads the tag; it has to be a pure function of source,
// not of who built it or when. A commit count would depend on clone depth and
// a timestamp is the one thing a reproducible build can never contain, so
// neither is used here - see docs/android-release.md for the full scheme.
//
// Tag shape: android-vMAJOR.MINOR.PATCH (e.g. android-v0.1.0), kept apart from
// the server's own vMAJOR.MINOR.PATCH tags in release.yml because the two ship
// on different schedules and a single "v0.1.0" would be ambiguous between them.
val androidVersionName: String = run {
    val fromProperty = findProperty("androidVersionName") as String?
    val fromTag = try {
        providers.exec {
            commandLine("git", "describe", "--tags", "--exact-match", "--match", "android-v*")
            workingDir = rootDir
            isIgnoreExitValue = true
        }.standardOutput.asText.get().trim().removePrefix("android-v")
    } catch (e: Exception) {
        ""
    }
    when {
        // -PandroidVersionName=x.y.z overrides the tag - useful for
        // `make android-reproducible`, which builds off a worktree that is
        // not itself checked out at a tag.
        !fromProperty.isNullOrBlank() -> fromProperty
        Regex("""\d+\.\d+\.\d+""").matches(fromTag) -> fromTag
        // HEAD is not exactly on an android-v tag: a local or CI dev build,
        // never what F-Droid builds. Loud rather than silently numbered 0.1.0
        // so nobody mistakes a dev build for a release. Patch 1, not 0: AGP
        // refuses versionCode 0 outright, and 0.0.0 would compute exactly that.
        else -> "0.0.1-dev"
    }
}

// versionCode as a pure function of versionName: MAJOR*1_000_000 +
// MINOR*1_000 + PATCH. Anyone - including F-Droid's own tooling - can
// recompute it from the tag alone without running this build.
fun versionCodeFor(name: String): Int {
    val parts = name.substringBefore('-').split(".").map { it.toIntOrNull() ?: 0 }
    val major = parts.getOrElse(0) { 0 }
    val minor = parts.getOrElse(1) { 0 }
    val patch = parts.getOrElse(2) { 0 }
    require(minor < 1000 && patch < 1000) {
        "versionName $name does not fit MAJOR*1_000_000 + MINOR*1_000 + PATCH"
    }
    return major * 1_000_000 + minor * 1_000 + patch
}

// Optional developer signing, read from a gitignored properties file.
//
// F-Droid signs the APK it publishes with its own key; this exists only so
// Adam can sign a release build himself and compare it against what F-Droid
// built, which is the entire point of a reproducible build being checkable.
// Never required: absent the file, `release` stays unsigned, which is the
// default F-Droid also builds from.
val keystorePropertiesFile = rootProject.file("keystore.properties")
val keystoreProperties = Properties().apply {
    if (keystorePropertiesFile.exists()) {
        FileInputStream(keystorePropertiesFile).use { load(it) }
    }
}

android {
    namespace = "com.lexarii.app"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.lexarii.app"
        // 26 covers Android 8 and later. Below that there is no usable TLS
        // story on devices that stopped getting updates years ago, and this
        // app's whole job is to hold a bearer token.
        minSdk = 26
        targetSdk = 35
        versionCode = versionCodeFor(androidVersionName)
        versionName = androidVersionName
        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }

    if (keystorePropertiesFile.exists()) {
        signingConfigs {
            create("release") {
                storeFile = file(keystoreProperties.getProperty("storeFile"))
                storePassword = keystoreProperties.getProperty("storePassword")
                keyAlias = keystoreProperties.getProperty("keyAlias")
                keyPassword = keystoreProperties.getProperty("keyPassword")
            }
        }
    }

    // AGP's own dependency-metadata blob: which SDKs and libraries built this
    // APK, Google-encrypted and normally embedded for Play's Integrity API. It
    // is proprietary by construction and an outright F-Droid rejection if
    // present, regardless of what this app actually depends on.
    dependenciesInfo {
        includeInApk = false
        includeInBundle = false
    }

    buildTypes {
        release {
            // Off for now, deliberately.
            //
            // F-Droid rebuilds this commit and compares the bytes, and a
            // shrinker is the part most likely to make that comparison fail
            // for reasons nobody can read. It goes on when there is enough
            // app for it to be worth the risk, with the mapping file kept.
            // No obfuscation either, which costs nothing here: the source is
            // published under the AGPL, so there is nothing to hide.
            isMinifyEnabled = false
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            // AGP embeds META-INF/version-control-info.textproto by default -
            // the git remote, branch and commit it was built from. Two people
            // building the same tagged commit can still disagree here: a
            // shallow clone, a detached HEAD, or (as in this worktree) a git
            // worktree instead of a plain checkout all change or blank out
            // what gets written, so the file is not a function of the source
            // tree alone. Off, for the same reason minification is off.
            vcsInfo {
                include = false
            }
            // Unsigned unless a developer keystore was supplied above - F-Droid
            // signs what it builds, and a build that only works pre-signed is
            // a build F-Droid cannot reproduce and compare.
            if (keystorePropertiesFile.exists()) {
                signingConfig = signingConfigs.getByName("release")
            }
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    buildFeatures {
        compose = true
        // No BuildConfig: nothing here branches on it, and it is a generated
        // class whose contents vary with the build.
        buildConfig = false
    }

    // The generated wire types live beside the hand-written source rather than
    // in build/, because they are committed: `make lint` checks them against
    // the Go, so a reviewer can see the contract without running a generator.
    sourceSets["main"].kotlin.srcDir("src/main/kotlin")
    sourceSets["test"].kotlin.srcDir("src/test/kotlin")

    packaging {
        resources {
            excludes += "/META-INF/{AL2.0,LGPL2.1}"
        }
    }
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
    }
}

// ReaderShellTest reads the shipped reader page straight off disk, which Gradle
// cannot see. Without declaring it, a change to reader.js or reader.html left
// the unit tests "up to date" - and a real failure on main was reported as a
// pass from the cache.
tasks.withType<Test>().configureEach {
    inputs.dir("src/main/assets").withPathSensitivity(PathSensitivity.RELATIVE)
}

dependencies {
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.lifecycle.runtime.ktx)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.androidx.activity.compose)
    implementation(libs.androidx.datastore.preferences)
    implementation(libs.androidx.work.runtime)

    implementation(platform(libs.compose.bom))
    implementation(libs.compose.ui)
    implementation(libs.compose.ui.tooling.preview)
    implementation(libs.compose.material3)
    debugImplementation(libs.compose.ui.tooling)

    implementation(libs.ktor.client.core)
    implementation(libs.ktor.client.okhttp)
    implementation(libs.ktor.client.content.negotiation)
    implementation(libs.ktor.serialization.kotlinx.json)
    implementation(libs.kotlinx.serialization.json)
    implementation(libs.kotlinx.coroutines.android)

    testImplementation(libs.junit)
    testImplementation(libs.ktor.client.mock)
    testImplementation(libs.kotlinx.coroutines.test)
}
