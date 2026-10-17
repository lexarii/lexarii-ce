// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import androidx.datastore.preferences.core.PreferenceDataStoreFactory
import androidx.datastore.preferences.core.doublePreferencesKey
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import com.lexarii.app.Settings
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.test.runTest
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.io.File

/** The typography the app sends the page, as agreed with reader.js. */
class TypographyTest {
    @get:Rule
    val tmp = TemporaryFolder()

    private fun argument(call: String): JsonObject {
        val json = call.substringAfter("Reader.setTypography(").removeSuffix(")")
        return Json.parseToJsonElement(json) as JsonObject
    }

    @Test
    fun `the call carries exactly the five agreed values, and is guarded`() {
        val call = typographyCall(
            ReadingSettings(ThemeChoice.SEPIA, FontChoice.SANS, 1.2, 1.5, MarginChoice.WIDE),
            systemDark = true,
        )
        assertTrue(call.startsWith("if (window.Reader && typeof Reader.setTypography === 'function') "))
        val value = argument(call)
        assertEquals(setOf("theme", "fontFamily", "fontScale", "lineHeight", "margin"), value.keys)
        assertEquals("sepia", value.getValue("theme").jsonPrimitive.content)
        assertEquals("sans", value.getValue("fontFamily").jsonPrimitive.content)
        assertEquals(1.2, value.getValue("fontScale").jsonPrimitive.content.toDouble(), 0.0)
        assertEquals(1.5, value.getValue("lineHeight").jsonPrimitive.content.toDouble(), 0.0)
        assertEquals("wide", value.getValue("margin").jsonPrimitive.content)
    }

    /** The page is only ever told one of its four themes. */
    @Test
    fun `following the system is light or dark as the phone is`() {
        assertEquals("dark", argument(typographyCall(ReadingSettings(), systemDark = true))["theme"]!!.jsonPrimitive.content)
        assertEquals("light", argument(typographyCall(ReadingSettings(), systemDark = false))["theme"]!!.jsonPrimitive.content)
        assertEquals("black", ReadingSettings(theme = ThemeChoice.BLACK).pageTheme(systemDark = false))
    }

    @Test
    fun `values outside the page's ranges are brought inside them`() {
        val wild = ReadingSettings(fontScale = 9.0, lineHeight = 0.2).normalised()
        assertEquals(1.6, wild.fontScale, 0.0)
        assertEquals(1.3, wild.lineHeight, 0.0)
        val odd = ReadingSettings(fontScale = 1.2345, lineHeight = Double.NaN).normalised()
        assertEquals(1.2, odd.fontScale, 0.0)
        assertEquals(ReadingSettings.DEFAULT_LINE_HEIGHT, odd.lineHeight, 0.0)
    }

    /** MUST match --page in reader.html for each theme; dark themes get light system-bar icons. */
    @Test
    fun `each theme has a page colour for the frame around the page`() {
        assertEquals(0xFFFBF8F1.toInt(), palette("light").page)
        assertEquals(0xFF16140F.toInt(), palette("dark").page)
        assertTrue(palette("dark").dark)
        assertTrue(palette("black").dark)
        assertFalse(palette("sepia").dark)
        assertFalse(palette("light").dark)
    }

    @Test
    fun `settings survive a restart, and anything unreadable in them falls back`() = runTest {
        val file = File(tmp.root, "settings.preferences_pb")
        val store = PreferenceDataStoreFactory.create(scope = backgroundScope) { file }
        val settings = Settings(store)
        assertEquals(ReadingSettings(), settings.reading.first())

        settings.save(ReadingSettings(ThemeChoice.BLACK, FontChoice.SANS, 1.4, 1.8, MarginChoice.NARROW))
        assertEquals(ReadingSettings(ThemeChoice.BLACK, FontChoice.SANS, 1.4, 1.8, MarginChoice.NARROW), settings.reading.first())

        store.edit {
            it[stringPreferencesKey("reader.theme")] = "neon"
            it[doublePreferencesKey("reader.fontScale")] = 40.0
        }
        val read = settings.reading.first()
        assertEquals(ThemeChoice.SYSTEM, read.theme)
        assertEquals(1.6, read.fontScale, 0.0)

        assertFalse(settings.unmeteredOnly.first())
        settings.setUnmeteredOnly(true)
        assertTrue(settings.unmeteredOnly.first())
    }
}
