// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import com.lexarii.app.offline.Mark
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.addJsonObject
import kotlinx.serialization.json.buildJsonArray
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

/*
 * The script the app runs in the reader's page. The boundary where text from
 * a book, a note, or somebody else's shared note meets code, so it lives on
 * its own, with nothing Android in it, where a JVM test can hold it to that.
 */

internal fun showCall(state: ReaderState.Showing): String {
    val target = buildJsonObject {
        when (val t = state.target) {
            is Target.Start -> Unit
            is Target.End -> put("end", true)
            is Target.Path -> put("path", t.path)
            is Target.Fragment -> put("fragment", t.id)
        }
    }
    // Every value goes in as JSON, so nothing in a chapter can close the
    // string it travels in and become code.
    return "Reader.show(${script(JsonPrimitive(state.html))}, ${state.spineIndex}, ${state.spineCount}, ${script(target)})"
}

/**
 * `Reader.setMarks` with every mark, as JSON: a note is typed by a person and
 * a shared one by somebody else, and neither may become code.
 */
internal fun marksCall(marks: List<Mark>): String {
    val list = buildJsonArray {
        for (m in marks) {
            addJsonObject {
                put("id", m.id)
                put("start", m.start)
                put("end", m.end)
                put("colour", m.colour?.let(::JsonPrimitive) ?: JsonNull)
                put("selectedText", m.selectedText?.let(::JsonPrimitive) ?: JsonNull)
                put("shared", m.shared)
                put("hasNote", m.hasNote)
            }
        }
    }
    return "Reader.setMarks(${script(list)})"
}

/**
 * `Reader.setTypography` with this device's settings, as the page and the app
 * agreed it: `{theme, fontFamily, fontScale, lineHeight, margin}`, every value
 * one the page accepts.
 *
 * Guarded, so a page without it - an older reader.js - is simply not restyled
 * rather than thrown at. The values are all the app's own, but they go in as
 * JSON all the same: one habit at this boundary, not two.
 */
internal fun typographyCall(settings: ReadingSettings, systemDark: Boolean): String {
    val s = settings.normalised()
    val value = buildJsonObject {
        put("theme", s.pageTheme(systemDark))
        put("fontFamily", s.font.key)
        put("fontScale", s.fontScale)
        put("lineHeight", s.lineHeight)
        put("margin", s.margin.key)
    }
    return "if (window.Reader && typeof Reader.setTypography === 'function') Reader.setTypography(${script(value)})"
}

/**
 * JSON as a script literal. JSON is script except for two characters, the
 * line and paragraph separators, which an engine older than ES2019 reads as
 * line ends inside a string - and a WebView on a phone nobody updates can be
 * that old. They can only occur inside strings, so escaping them is exact.
 */
private fun script(value: JsonElement): String =
    value.toString().replace("\u2028", "\\u2028").replace("\u2029", "\\u2029")
