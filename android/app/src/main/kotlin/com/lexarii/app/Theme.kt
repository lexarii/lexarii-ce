// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app

import android.os.Build
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.platform.LocalContext

/**
 * Light or dark as the system says, in the wallpaper's colours where Android
 * offers them (12 and later) and the platform defaults otherwise.
 *
 * The reader follows the same setting with its own paper-and-ink palette; this
 * is what keeps the shelf around it from being the one white screen at night.
 */
@Composable
fun LexariiTheme(content: @Composable () -> Unit) {
    val dark = isSystemInDarkTheme()
    val colours = when {
        Build.VERSION.SDK_INT >= Build.VERSION_CODES.S -> {
            val context = LocalContext.current
            if (dark) dynamicDarkColorScheme(context) else dynamicLightColorScheme(context)
        }
        dark -> darkColorScheme()
        else -> lightColorScheme()
    }
    MaterialTheme(colorScheme = colours, content = content)
}
