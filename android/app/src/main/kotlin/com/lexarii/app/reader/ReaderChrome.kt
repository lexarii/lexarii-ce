// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import android.app.Activity
import android.content.Context
import android.content.ContextWrapper
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.tween
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.sizeIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.LocalContentColor
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Slider
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalView
import androidx.compose.ui.unit.dp
import androidx.core.view.WindowCompat
import kotlinx.coroutines.delay
import kotlin.math.roundToInt

/**
 * Material colours to match the page: light or dark with the page's own
 * colour behind everything, so the notices, sheets and buttons over the page
 * are legible on sepia and on black as much as on the default.
 */
@Composable
fun ReaderTheme(palette: PagePalette, content: @Composable () -> Unit) {
    val page = Color(palette.page)
    val base = if (palette.dark) darkColorScheme() else lightColorScheme()
    val scheme = base.copy(background = page, surface = page)
    MaterialTheme(colorScheme = scheme) {
        // Text drawn straight onto the page - "Opening…", an error - has no
        // Surface to take its ink from, and would be black on a black page.
        CompositionLocalProvider(LocalContentColor provides scheme.onBackground, content = content)
    }
}

/**
 * The status and navigation bar icons dark on a light page and light on a
 * dark one, put back to the phone's own setting when the reader closes.
 */
@Composable
fun SystemBarsFor(palette: PagePalette, systemDark: Boolean) {
    val view = LocalView.current
    DisposableEffect(palette.dark, systemDark) {
        val window = view.context.activity()?.window
        val bars = window?.let { WindowCompat.getInsetsController(it, view) }
        bars?.isAppearanceLightStatusBars = !palette.dark
        bars?.isAppearanceLightNavigationBars = !palette.dark
        onDispose {
            bars?.isAppearanceLightStatusBars = !systemDark
            bars?.isAppearanceLightNavigationBars = !systemDark
        }
    }
}

private tailrec fun Context.activity(): Activity? = when (this) {
    is Activity -> this
    is ContextWrapper -> baseContext.activity()
    else -> null
}

/**
 * The reader's controls: back to the library, the typography sheet, and
 * whatever the reader adds - a PDF's contents and page number.
 *
 * Shown by a tap in the middle of the page. There is always a way in without
 * that tap too: a small control in the corner that fades back after a few
 * seconds but never goes, so the settings are there even for a page that
 * never says it was tapped.
 */
@Composable
fun ReaderControls(
    shown: Boolean,
    onToggle: () -> Unit,
    onBack: () -> Unit,
    onSettings: () -> Unit,
    modifier: Modifier = Modifier,
    extra: @Composable () -> Unit = {},
) {
    if (shown) {
        // A Surface rather than a background, so text in the bar - a PDF's
        // page number - takes the ink that goes with it, not black on black.
        Surface(color = MaterialTheme.colorScheme.surfaceContainerHigh, modifier = modifier.fillMaxWidth()) {
            Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.padding(horizontal = 4.dp)) {
                TextButton(onClick = onBack) { Text("‹ Library") }
                Spacer(Modifier.weight(1f))
                extra()
                TextButton(onClick = onSettings) { Text("Aa") }
                TextButton(onClick = onToggle) { Text("✕") }
            }
        }
    } else {
        var settled by remember { mutableStateOf(false) }
        LaunchedEffect(Unit) {
            delay(FADE_AFTER_MS)
            settled = true
        }
        val alpha by animateFloatAsState(if (settled) 0.35f else 1f, tween(600), label = "controls")
        Box(modifier = modifier.fillMaxWidth(), contentAlignment = Alignment.TopEnd) {
            TextButton(onClick = onToggle, modifier = Modifier.alpha(alpha).sizeIn(minWidth = 48.dp, minHeight = 48.dp)) {
                Text("⋯", style = MaterialTheme.typography.titleMedium)
            }
        }
    }
}

private const val FADE_AFTER_MS = 3_000L

/**
 * The typography sheet. Every change is saved as it is made, and the page
 * takes it at once; a slider saves when it is let go, since each save lays
 * the chapter out again.
 *
 * `text` is false for a PDF: its pages are pictures of type already set,
 * and only the colours around them are the reader's to choose.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun TypographySheet(
    settings: ReadingSettings,
    text: Boolean,
    onChange: (ReadingSettings) -> Unit,
    onDismiss: () -> Unit,
) {
    ModalBottomSheet(onDismissRequest = onDismiss, sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)) {
        Column(
            modifier = Modifier
                .fillMaxWidth()
                .verticalScroll(rememberScrollState())
                .padding(start = 24.dp, end = 24.dp, bottom = 24.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            Text("Theme", style = MaterialTheme.typography.labelLarge)
            Choices(
                listOf(
                    ThemeChoice.SYSTEM to "Follow system",
                    ThemeChoice.LIGHT to "Light",
                    ThemeChoice.SEPIA to "Sepia",
                    ThemeChoice.DARK to "Dark",
                    ThemeChoice.BLACK to "Black",
                ),
                settings.theme,
            ) { onChange(settings.copy(theme = it)) }

            if (text) {
                Text("Font", style = MaterialTheme.typography.labelLarge)
                Choices(listOf(FontChoice.SERIF to "Serif", FontChoice.SANS to "Sans"), settings.font) {
                    onChange(settings.copy(font = it))
                }

                Stepped(
                    label = "Size",
                    value = settings.fontScale,
                    range = ReadingSettings.MIN_SCALE..ReadingSettings.MAX_SCALE,
                    shown = { "${(it * 100).roundToInt()}%" },
                ) { onChange(settings.copy(fontScale = it)) }

                Stepped(
                    label = "Line spacing",
                    value = settings.lineHeight,
                    range = ReadingSettings.MIN_LINE..ReadingSettings.MAX_LINE,
                    shown = { "%.1f".format(it) },
                ) { onChange(settings.copy(lineHeight = it)) }

                Text("Margins", style = MaterialTheme.typography.labelLarge)
                Choices(
                    listOf(MarginChoice.NARROW to "Narrow", MarginChoice.NORMAL to "Normal", MarginChoice.WIDE to "Wide"),
                    settings.margin,
                ) { onChange(settings.copy(margin = it)) }
            }
        }
    }
}

@Composable
private fun <T> Choices(options: List<Pair<T, String>>, selected: T, onPick: (T) -> Unit) {
    Row(horizontalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.horizontalScroll(rememberScrollState())) {
        for ((value, label) in options) {
            FilterChip(selected = value == selected, onClick = { onPick(value) }, label = { Text(label) })
        }
    }
}

/** A slider in tenths, saved when let go. */
@Composable
private fun Stepped(
    label: String,
    value: Double,
    range: ClosedFloatingPointRange<Double>,
    shown: (Double) -> String,
    onDone: (Double) -> Unit,
) {
    var dragging by remember(value) { mutableFloatStateOf(value.toFloat()) }
    val tenths = ((range.endInclusive - range.start) * 10).roundToInt()
    Row(verticalAlignment = Alignment.CenterVertically) {
        Text(label, style = MaterialTheme.typography.labelLarge, modifier = Modifier.weight(1f))
        Text(shown((dragging * 10).roundToInt() / 10.0), style = MaterialTheme.typography.bodyMedium)
    }
    Slider(
        value = dragging,
        onValueChange = { dragging = it },
        valueRange = range.start.toFloat()..range.endInclusive.toFloat(),
        steps = tenths - 1,
        onValueChangeFinished = { onDone((dragging * 10).roundToInt() / 10.0) },
    )
}
