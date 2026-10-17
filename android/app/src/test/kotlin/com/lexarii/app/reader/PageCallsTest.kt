// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import com.lexarii.app.offline.Mark
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The script run in the reader's page, where a chapter, a note, and somebody
 * else's shared note meet code. Whatever they hold, each must arrive as one
 * JSON value and leave as the same text - never as script of its own.
 */
class PageCallsTest {
    private val hostile = listOf(
        "\"); alert(1); (\"",
        "'); alert(1); ('",
        "back\\slash\\\" and a quote",
        "</script><script>alert(1)</script>",
        "line\u2028separator and paragraph\u2029separator",
        "\n\r\t\u0000 control",
        "`${'$'}{alert(1)}`",
        "\uD83D\uDCD6 and a lone \uD83D",
    )

    /** The one argument between `prefix(` and the final `)`, which must be JSON and nothing more. */
    private fun argument(call: String, prefix: String): String {
        assertTrue(call, call.startsWith("$prefix(") && call.endsWith(")"))
        return call.substring(prefix.length + 1, call.length - 1)
    }

    @Test
    fun `every mark field reaches the page as the text it was`() {
        val marks = hostile.mapIndexed { i, text ->
            Mark(
                id = "id-$i-$text", bookFileId = "f", type = "note", start = "epub:0/p[1]:0", end = "epub:0/p[1]:5",
                selectedText = text, note = text, colour = text, shared = i % 2 == 0,
                createdAt = "2026-09-24T20:00:00Z", updatedAt = "2026-09-24T20:00:00Z",
            )
        }
        val call = marksCall(marks)

        // Parsed whole, as one value: anything that closed a string and went
        // on as script would not parse, or would leave text over.
        val list = Json.parseToJsonElement(argument(call, "Reader.setMarks")) as JsonArray
        assertEquals(hostile.size, list.size)
        for ((i, element) in list.withIndex()) {
            val fields = element as JsonObject
            assertEquals(hostile[i], fields.getValue("selectedText").jsonPrimitive.content)
            assertEquals(hostile[i], fields.getValue("colour").jsonPrimitive.content)
            assertEquals("id-$i-${hostile[i]}", fields.getValue("id").jsonPrimitive.content)
            assertFalse("the note body itself never goes to the page", fields.containsKey("note"))
        }
        assertFalse("a raw line separator ends a string in an older engine", '\u2028' in call || '\u2029' in call)
    }

    @Test
    fun `a chapter reaches the page as the text it was`() {
        for (html in hostile) {
            val call = showCall(ReaderState.Showing(html, 3, 9, Target.Path("p[2]\"); alert(1); (\""), 1))
            assertFalse('\u2028' in call || '\u2029' in call)
            val args = Json.parseToJsonElement("[" + argument(call, "Reader.show") + "]") as JsonArray
            assertEquals(4, args.size)
            assertEquals(html, args[0].jsonPrimitive.content)
            assertEquals("p[2]\"); alert(1); (\"", (args[3] as JsonObject).getValue("path").jsonPrimitive.content)
        }
    }
}
