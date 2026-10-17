// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import kotlin.math.roundToInt

/** The reader's colours. SYSTEM is light or dark as the phone is. */
enum class ThemeChoice(val key: String) {
    SYSTEM("system"),
    LIGHT("light"),
    SEPIA("sepia"),
    DARK("dark"),
    BLACK("black");

    companion object {
        fun of(key: String?): ThemeChoice = entries.firstOrNull { it.key == key } ?: SYSTEM
    }
}

enum class FontChoice(val key: String) {
    SERIF("serif"),
    SANS("sans");

    companion object {
        fun of(key: String?): FontChoice = entries.firstOrNull { it.key == key } ?: SERIF
    }
}

enum class MarginChoice(val key: String) {
    NARROW("narrow"),
    NORMAL("normal"),
    WIDE("wide");

    companion object {
        fun of(key: String?): MarginChoice = entries.firstOrNull { it.key == key } ?: NORMAL
    }
}

/**
 * How this device lays out a book's text. Kept per device rather than per
 * account: it is about this screen and these eyes, and it has nothing in it
 * anybody signing in after would mind inheriting.
 *
 * The ranges are the page's, agreed with reader.js: `Reader.setTypography`
 * takes a font scale from 0.8 to 1.6 and a line height from 1.3 to 1.9, and
 * values are always brought inside them here rather than trusted to be.
 */
data class ReadingSettings(
    val theme: ThemeChoice = ThemeChoice.SYSTEM,
    val font: FontChoice = FontChoice.SERIF,
    val fontScale: Double = 1.0,
    val lineHeight: Double = DEFAULT_LINE_HEIGHT,
    val margin: MarginChoice = MarginChoice.NORMAL,
) {
    /** Inside the ranges, and on the tenths the controls step in. */
    fun normalised(): ReadingSettings = copy(
        fontScale = tenths(fontScale, MIN_SCALE, MAX_SCALE, 1.0),
        lineHeight = tenths(lineHeight, MIN_LINE, MAX_LINE, DEFAULT_LINE_HEIGHT),
    )

    /** The page's theme: SYSTEM resolved, so the page is only ever told one of its four. */
    fun pageTheme(systemDark: Boolean): String = when (theme) {
        ThemeChoice.SYSTEM -> if (systemDark) ThemeChoice.DARK.key else ThemeChoice.LIGHT.key
        else -> theme.key
    }

    companion object {
        const val MIN_SCALE = 0.8
        const val MAX_SCALE = 1.6
        const val MIN_LINE = 1.3
        const val MAX_LINE = 1.9

        // What reader.html sets before anything is chosen.
        const val DEFAULT_LINE_HEIGHT = 1.6

        private fun tenths(value: Double, min: Double, max: Double, fallback: Double): Double {
            if (!value.isFinite()) return fallback
            return (value.coerceIn(min, max) * 10).roundToInt() / 10.0
        }
    }
}

/** A theme's page colour, as ARGB, and whether its ink is light. */
data class PagePalette(val page: Int, val dark: Boolean)

/**
 * The page colour behind each theme, for everything the app draws around the
 * page - the margins the system bars leave, the WebView before it paints, the
 * PDF reader's surround - so none of it frames the page in another colour.
 *
 * MUST match `--page` for each theme in assets/reader/reader.html.
 */
fun palette(theme: String): PagePalette = when (theme) {
    "sepia" -> PagePalette(0xFFF4ECD8.toInt(), dark = false)
    "dark" -> PagePalette(0xFF16140F.toInt(), dark = true)
    "black" -> PagePalette(0xFF000000.toInt(), dark = true)
    else -> PagePalette(0xFFFBF8F1.toInt(), dark = false)
}
