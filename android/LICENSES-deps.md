<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# Android client: dependency licence audit

Every entry below is a module resolved onto `:app:releaseRuntimeClasspath` -
the exact classpath a release APK ships. Nothing here comes from a proguard
mapping or a `testImplementation`-only path; those never reach the device.

Regenerate the list this table is built from with:

```
cd android
ANDROID_HOME=<sdk> JAVA_HOME=<jdk17> ./gradlew :app:dependencies --configuration releaseRuntimeClasspath
```

Every artifact was checked directly - the `<licenses>` block of its own POM,
or in one case (`slf4j-api`) the `META-INF/LICENSE.txt` bundled in the jar
itself - not asserted from memory. **All 151 resolved artifacts carry
Apache-2.0 or MIT. Nothing proprietary, nothing source-available-only,
nothing with a non-commercial clause. Both licences are OSI-approved
permissive licences and are AGPL-3.0-compatible: they add no restriction
beyond attribution, so combining them into an AGPL work is unrestricted.**

## By licence

| Licence | Artifacts |
|---|---|
| Apache-2.0 | 150 |
| MIT | 1 (`org.slf4j:slf4j-api`) |

## Flagged for a reason, not for a licence problem

Everything below is Apache-2.0 and unproblematic on licence grounds. They are
called out because a reviewer (F-Droid's or ours) would reasonably ask what
they do and whether they phone home. None of them do.

- **`androidx.profileinstaller:profileinstaller` / `androidx.startup:startup-runtime`.**
  Neither is a direct dependency - both arrive transitively, brought in by
  Compose. `profileinstaller` writes AOT-compilation hints (a "baseline
  profile") to the app's own private storage on first run, using
  `androidx.startup`'s `ContentProvider`-based initializer to do it without an
  extra `Application.onCreate()` override. It reads nothing off-device, sends
  nothing over the network, and touches nothing outside
  `getFilesDir()`/`getCodeCacheDir()`. Standard in any Compose app; no client
  code here references it directly. Also see "Baseline profiles" in
  `docs/android-release.md` for what it means for reproducibility.
- **Kotlin/Compose tooling** (`org.jetbrains.kotlin:kotlin-stdlib*`,
  `org.jetbrains.kotlinx:kotlinx-coroutines-*`, `kotlinx-serialization-*`,
  `kotlinx-io-*`, `androidx.compose.*`, `androidx.lifecycle:*`). All
  JetBrains/AndroidX, all Apache-2.0, all libre. `kotlin-android-extensions-runtime`
  and `kotlin-parcelize-runtime` are pulled in by the Kotlin Gradle plugins
  applied in `build.gradle.kts` (`kotlin.plugin.compose`,
  `kotlin.plugin.serialization`) rather than by anything this app calls
  directly; both are the Kotlin project's own Apache-2.0 runtime support jars.
- **`com.google.guava:listenablefuture:1.0`.** Despite the `com.google.guava`
  group, this is not Guava itself - it is a deliberately empty jar (one
  interface, no implementation) that the Guava project publishes so consumers
  who want only `ListenableFuture` do not pull all of Guava. Its own embedded
  POM declares `<parent>com.google.guava:guava-parent:26.0-android</parent>`
  rather than repeating a `<licenses>` block, and `guava-parent` is
  Apache-2.0 - the same licence as Guava proper. It is a transitive dependency
  of `androidx.datastore`, not something this app calls.
- **WorkManager** (`androidx.work:work-runtime`) **and what it brings: Room
  and SQLite** (`androidx.room:*`, `androidx.sqlite:*`), `lifecycle-livedata`,
  `lifecycle-service`, `tracing`. The one direct dependency here added for a
  feature rather than for the toolkit: keeping a book on the device carries on
  after the app is left, and resumes after Android stops the process. It keeps
  its queue in a SQLite database of its own (Room) in the app's private
  `no_backup` directory, re-schedules after a reboot (its manifest merges in
  `RECEIVE_BOOT_COMPLETED`), and runs long downloads as a `dataSync`
  foreground service. It talks to nothing: the only network traffic is the
  app's own download from the person's own server. `androidx.work:work-gcm` -
  the one piece of WorkManager that touches Google Play services - is a
  separate artifact and is not used. About 450 KB of dex (work 294 KB, Room
  118 KB, SQLite 20 KB, the rest small), minification off.
- **OkHttp / Okio** (`com.squareup.okhttp3:okhttp*`, `com.squareup.okio:okio*`).
  Square's HTTP and I/O libraries, brought in as Ktor's OkHttp engine
  (`ktor-client-okhttp`), chosen in `libs.versions.toml` specifically because
  the app talks to a self-hosted server as often over plain HTTP on a LAN as
  over TLS. Apache-2.0.

## Full resolved classpath

| Artifact | Version | Licence |
|---|---|---|
| `androidx.activity:activity` | 1.9.3 | Apache-2.0 |
| `androidx.activity:activity-compose` | 1.9.3 | Apache-2.0 |
| `androidx.activity:activity-ktx` | 1.7.0 | Apache-2.0 |
| `androidx.annotation:annotation` | 1.1.0 | Apache-2.0 |
| `androidx.annotation:annotation-experimental` | 1.4.0 | Apache-2.0 |
| `androidx.annotation:annotation-jvm` | 1.8.1 | Apache-2.0 |
| `androidx.arch.core:core-common` | 2.2.0 | Apache-2.0 |
| `androidx.arch.core:core-runtime` | 2.2.0 | Apache-2.0 |
| `androidx.autofill:autofill` | 1.0.0 | Apache-2.0 |
| `androidx.collection:collection` | 1.0.0 | Apache-2.0 |
| `androidx.collection:collection-jvm` | 1.4.4 | Apache-2.0 |
| `androidx.collection:collection-ktx` | 1.4.4 | Apache-2.0 |
| `androidx.compose:compose-bom` | 2024.10.01 | Apache-2.0 |
| `androidx.compose.animation:animation` | 1.7.5 | Apache-2.0 |
| `androidx.compose.animation:animation-android` | 1.7.5 | Apache-2.0 |
| `androidx.compose.animation:animation-core` | 1.7.5 | Apache-2.0 |
| `androidx.compose.animation:animation-core-android` | 1.7.5 | Apache-2.0 |
| `androidx.compose.foundation:foundation` | 1.7.5 | Apache-2.0 |
| `androidx.compose.foundation:foundation-android` | 1.7.5 | Apache-2.0 |
| `androidx.compose.foundation:foundation-layout` | 1.7.5 | Apache-2.0 |
| `androidx.compose.foundation:foundation-layout-android` | 1.7.5 | Apache-2.0 |
| `androidx.compose.material:material-icons-core` | 1.7.5 | Apache-2.0 |
| `androidx.compose.material:material-icons-core-android` | 1.7.5 | Apache-2.0 |
| `androidx.compose.material:material-ripple` | 1.7.5 | Apache-2.0 |
| `androidx.compose.material:material-ripple-android` | 1.7.5 | Apache-2.0 |
| `androidx.compose.material3:material3` | 1.3.1 | Apache-2.0 |
| `androidx.compose.material3:material3-android` | 1.3.1 | Apache-2.0 |
| `androidx.compose.runtime:runtime` | 1.6.0 | Apache-2.0 |
| `androidx.compose.runtime:runtime-android` | 1.7.5 | Apache-2.0 |
| `androidx.compose.runtime:runtime-saveable` | 1.7.5 | Apache-2.0 |
| `androidx.compose.runtime:runtime-saveable-android` | 1.7.5 | Apache-2.0 |
| `androidx.compose.ui:ui` | 1.6.0 | Apache-2.0 |
| `androidx.compose.ui:ui-android` | 1.7.5 | Apache-2.0 |
| `androidx.compose.ui:ui-geometry` | 1.7.5 | Apache-2.0 |
| `androidx.compose.ui:ui-geometry-android` | 1.7.5 | Apache-2.0 |
| `androidx.compose.ui:ui-graphics` | 1.7.5 | Apache-2.0 |
| `androidx.compose.ui:ui-graphics-android` | 1.7.5 | Apache-2.0 |
| `androidx.compose.ui:ui-text` | 1.7.5 | Apache-2.0 |
| `androidx.compose.ui:ui-text-android` | 1.7.5 | Apache-2.0 |
| `androidx.compose.ui:ui-tooling-preview` | 1.7.5 | Apache-2.0 |
| `androidx.compose.ui:ui-tooling-preview-android` | 1.7.5 | Apache-2.0 |
| `androidx.compose.ui:ui-unit` | 1.7.5 | Apache-2.0 |
| `androidx.compose.ui:ui-unit-android` | 1.7.5 | Apache-2.0 |
| `androidx.compose.ui:ui-util` | 1.7.5 | Apache-2.0 |
| `androidx.compose.ui:ui-util-android` | 1.7.5 | Apache-2.0 |
| `androidx.concurrent:concurrent-futures` | 1.0.0 | Apache-2.0 |
| `androidx.concurrent:concurrent-futures-ktx` | 1.1.0 | Apache-2.0 |
| `androidx.core:core` | 1.13.1 | Apache-2.0 |
| `androidx.core:core-ktx` | 1.13.1 | Apache-2.0 |
| `androidx.customview:customview-poolingcontainer` | 1.0.0 | Apache-2.0 |
| `androidx.datastore:datastore` | 1.1.1 | Apache-2.0 |
| `androidx.datastore:datastore-android` | 1.1.1 | Apache-2.0 |
| `androidx.datastore:datastore-core` | 1.1.1 | Apache-2.0 |
| `androidx.datastore:datastore-core-android` | 1.1.1 | Apache-2.0 |
| `androidx.datastore:datastore-core-okio` | 1.1.1 | Apache-2.0 |
| `androidx.datastore:datastore-core-okio-jvm` | 1.1.1 | Apache-2.0 |
| `androidx.datastore:datastore-preferences` | 1.1.1 | Apache-2.0 |
| `androidx.datastore:datastore-preferences-android` | 1.1.1 | Apache-2.0 |
| `androidx.datastore:datastore-preferences-core` | 1.1.1 | Apache-2.0 |
| `androidx.datastore:datastore-preferences-core-jvm` | 1.1.1 | Apache-2.0 |
| `androidx.emoji2:emoji2` | 1.2.0 | Apache-2.0 |
| `androidx.graphics:graphics-path` | 1.0.1 | Apache-2.0 |
| `androidx.interpolator:interpolator` | 1.0.0 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-common` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-common-java8` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-common-jvm` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-livedata` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-livedata-core` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-livedata-core-ktx` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-process` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-runtime` | 2.6.2 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-runtime-android` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-runtime-compose` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-runtime-compose-android` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-runtime-ktx` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-runtime-ktx-android` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-service` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-viewmodel` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-viewmodel-android` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-viewmodel-compose` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-viewmodel-compose-android` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-viewmodel-ktx` | 2.8.7 | Apache-2.0 |
| `androidx.lifecycle:lifecycle-viewmodel-savedstate` | 2.8.7 | Apache-2.0 |
| `androidx.profileinstaller:profileinstaller` | 1.3.1 | Apache-2.0 |
| `androidx.room:room-common` | 2.6.1 | Apache-2.0 |
| `androidx.room:room-ktx` | 2.6.1 | Apache-2.0 |
| `androidx.room:room-runtime` | 2.6.1 | Apache-2.0 |
| `androidx.savedstate:savedstate` | 1.2.1 | Apache-2.0 |
| `androidx.savedstate:savedstate-ktx` | 1.2.1 | Apache-2.0 |
| `androidx.sqlite:sqlite` | 2.4.0 | Apache-2.0 |
| `androidx.sqlite:sqlite-framework` | 2.4.0 | Apache-2.0 |
| `androidx.startup:startup-runtime` | 1.1.1 | Apache-2.0 |
| `androidx.tracing:tracing` | 1.2.0 | Apache-2.0 |
| `androidx.tracing:tracing-ktx` | 1.2.0 | Apache-2.0 |
| `androidx.versionedparcelable:versionedparcelable` | 1.1.1 | Apache-2.0 |
| `androidx.work:work-runtime` | 2.10.5 | Apache-2.0 |
| `com.google.guava:listenablefuture` | 1.0 | Apache-2.0 |
| `com.squareup.okhttp3:okhttp` | 4.12.0 | Apache-2.0 |
| `com.squareup.okhttp3:okhttp-sse` | 4.12.0 | Apache-2.0 |
| `com.squareup.okio:okio` | 3.4.0 | Apache-2.0 |
| `com.squareup.okio:okio-jvm` | 3.9.1 | Apache-2.0 |
| `io.ktor:ktor-client-content-negotiation` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-client-content-negotiation-jvm` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-client-core` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-client-core-jvm` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-client-okhttp` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-client-okhttp-jvm` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-events` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-events-jvm` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-http` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-http-jvm` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-io` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-io-jvm` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-serialization` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-serialization-jvm` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-serialization-kotlinx` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-serialization-kotlinx-json` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-serialization-kotlinx-json-jvm` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-serialization-kotlinx-jvm` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-sse` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-sse-jvm` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-utils` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-utils-jvm` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-websockets` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-websocket-serialization` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-websocket-serialization-jvm` | 3.0.1 | Apache-2.0 |
| `io.ktor:ktor-websockets-jvm` | 3.0.1 | Apache-2.0 |
| `org.jetbrains:annotations` | 13.0 | Apache-2.0 |
| `org.jetbrains.kotlin:kotlin-android-extensions-runtime` | 1.9.22 | Apache-2.0 |
| `org.jetbrains.kotlin:kotlin-parcelize-runtime` | 1.9.22 | Apache-2.0 |
| `org.jetbrains.kotlin:kotlin-stdlib` | 2.0.21 | Apache-2.0 |
| `org.jetbrains.kotlin:kotlin-stdlib-common` | 2.0.21 | Apache-2.0 |
| `org.jetbrains.kotlin:kotlin-stdlib-jdk7` | 1.8.22 | Apache-2.0 |
| `org.jetbrains.kotlin:kotlin-stdlib-jdk8` | 1.8.22 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-coroutines-android` | 1.9.0 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-coroutines-bom` | 1.9.0 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-coroutines-core` | 1.7.3 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-coroutines-core-jvm` | 1.9.0 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-coroutines-slf4j` | 1.9.0 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-io-bytestring` | 0.5.4 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-io-bytestring-jvm` | 0.5.4 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-io-core` | 0.5.4 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-io-core-jvm` | 0.5.4 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-serialization-bom` | 1.7.3 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-serialization-core` | 1.7.3 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-serialization-core-jvm` | 1.7.3 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-serialization-json` | 1.7.3 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-serialization-json-io` | 1.7.3 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-serialization-json-io-jvm` | 1.7.3 | Apache-2.0 |
| `org.jetbrains.kotlinx:kotlinx-serialization-json-jvm` | 1.7.3 | Apache-2.0 |
| `org.slf4j:slf4j-api` | 2.0.16 | MIT (confirmed from `META-INF/LICENSE.txt` inside the jar - the resolved POM has no `<licenses>` block) |

## Build-time only (not on the runtime classpath, not shipped)

The Gradle/Kotlin/AGP plugins that build the app - `com.android.application`,
`org.jetbrains.kotlin.android`, `kotlin.plugin.serialization`,
`kotlin.plugin.compose` - run on the build machine and are not present in the
APK. They were not included above for the same reason `goose` is noted as
"build-time only" in the root `NOTICE`: what matters for licensing an
application is what ships inside it.

## Repositories

`android/settings.gradle.kts` resolves only from `google()` (scoped to
`com.android.*`, `com.google.*`, `androidx.*` groups) and `mavenCentral()` -
both accepted by F-Droid - and `dependencyResolutionManagement` refuses any
repository declared by a subproject (`FAIL_ON_PROJECT_REPOS`), so this list is
complete: nothing in the build can silently resolve from anywhere else.
