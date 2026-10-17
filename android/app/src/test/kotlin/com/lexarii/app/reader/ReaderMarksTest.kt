// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import com.lexarii.app.offline.Delivery
import com.lexarii.app.testing.FakeServer
import com.lexarii.app.testing.annotationJson
import com.lexarii.app.testing.device
import com.lexarii.app.testing.pullPage
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import kotlinx.serialization.json.JsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.time.Instant

/**
 * What the page says about marks, checked: it is a WebView holding untrusted
 * HTML, and nothing it hands over is kept or sent until it has been.
 */
class ReaderMarksTest {
    @get:Rule
    val tmp = TemporaryFolder()

    private val markA = "0b5d1e8a-0000-4000-8000-00000000000a"
    private var now = Instant.parse("2026-09-24T20:00:00Z")

    private fun TestScope.opened(server: FakeServer = FakeServer()): ReaderController {
        val device = device(tmp.root)
        val reader = ReaderController(
            server.bookFileId, server.client(this), device.kept, device.outbox, device.marks, device.sync, this,
            clock = { now }, newId = { markA },
        )
        reader.open()
        advanceUntilIdle()
        return reader
    }

    private fun selection(start: String, end: String, text: String = "some words") =
        """{"start":${JsonPrimitive(start)},"end":${JsonPrimitive(end)},"selectedText":${JsonPrimitive(text)}}"""

    @Test
    fun `a selection that is not a place in the chapter on screen is refused`() = runTest {
        val server = FakeServer()
        val reader = opened(server)
        val refused = listOf(
            null,
            "",
            "null",
            "not json",
            "[]",
            "{}",
            "42",
            // Another chapter, or no chapter at all.
            selection("epub:2/p[1]:0", "epub:2/p[1]:5"),
            selection("epub:0/p[1]:0", "epub:1/p[1]:5"),
            selection("javascript:alert(1)", "epub:0/p[1]:5"),
            selection("pdf:1:0", "pdf:1:5"),
            selection("epub:0/a:b:0", "epub:0/p[1]:5"),
            selection("epub:0/p[1]:99999999999", "epub:0/p[1]:5"),
            // Backwards, or nothing, within one element.
            selection("epub:0/p[1]:9", "epub:0/p[1]:2"),
            selection("epub:0/p[1]:4", "epub:0/p[1]:4"),
            // No words.
            selection("epub:0/p[1]:0", "epub:0/p[1]:5", text = "   "),
            """{"start":"epub:0/p[1]:0","end":"epub:0/p[1]:5"}""",
            """{"start":"epub:0/p[1]:0","end":"epub:0/p[1]:5","selectedText":7}""",
            """{"start":["epub:0/p[1]:0"],"end":"epub:0/p[1]:5","selectedText":"x"}""",
            // A path nobody's book has, sent to be stored and synced.
            selection("epub:0/" + "div[1]/".repeat(1000) + "p[1]:0", "epub:0/p[1]:5"),
        )
        for (raw in refused) assertFalse("accepted $raw", reader.highlight(raw))
        advanceUntilIdle()
        assertTrue(reader.marks.value.isEmpty())
        assertTrue(server.pushed.isEmpty())
    }

    @Test
    fun `a selection in this chapter becomes a highlight, sent at once`() = runTest {
        val server = FakeServer()
        val reader = opened(server)

        assertTrue(reader.highlight(selection("epub:0/div[1]/p[2]:3", "epub:0/div[1]/p[4]:10", "  across paragraphs ")))
        advanceUntilIdle()

        val mark = reader.marks.value.single()
        assertEquals("highlight", mark.type)
        assertEquals("across paragraphs", mark.selectedText)
        assertEquals(Delivery.SENT, mark.delivery)
        assertEquals(markA, server.pushedMarks.single().id)
        assertNull("a highlight alone opens nothing", reader.openMark.value)
    }

    /** evaluateJavascript hands back the JSON of what was returned; a string is one more layer. */
    @Test
    fun `a selection handed back as a string is read the same`() = runTest {
        val reader = opened()
        assertTrue(reader.highlight(JsonPrimitive(selection("epub:0/p[1]:0", "epub:0/p[1]:5")).toString()))
    }

    @Test
    fun `a very long selection keeps its anchor and a capped snapshot`() = runTest {
        val reader = opened()
        // An emoji straddling the cap must not be cut in half.
        val long = "a".repeat(ReaderController.MAX_SELECTED_TEXT - 1) + "📖" + "b".repeat(50)
        assertTrue(reader.highlight(selection("epub:0/p[1]:0", "epub:0/p[90]:5", long)))
        advanceUntilIdle()
        val text = reader.marks.value.single().selectedText!!
        assertEquals(ReaderController.MAX_SELECTED_TEXT - 1, text.length)
        assertFalse(Character.isHighSurrogate(text.last()))
    }

    /** The server's own rule for an edit, applied here because its push takes the type as sent. */
    @Test
    fun `writing a note makes it a note, and clearing it makes it a highlight again`() = runTest {
        val server = FakeServer()
        val reader = opened(server)

        assertTrue(reader.highlight(selection("epub:0/p[1]:0", "epub:0/p[1]:5"), writing = true))
        advanceUntilIdle()
        assertEquals(OpenMark(markA, writing = true), reader.openMark.value)

        now = Instant.parse("2026-09-24T20:05:00Z")
        reader.updateNote(markA, "  a thought \n")
        advanceUntilIdle()
        assertNull(reader.openMark.value)
        reader.marks.value.single().let {
            assertEquals("note", it.type)
            assertEquals("a thought", it.note)
            assertTrue(it.hasNote)
        }
        server.pushedMarks.last().let {
            assertEquals("note", it.type)
            assertEquals("a thought", it.note)
            assertEquals("2026-09-24T20:05:00Z", it.updatedAt)
        }

        now = Instant.parse("2026-09-24T20:06:00Z")
        reader.updateNote(markA, "  \n ")
        advanceUntilIdle()
        reader.marks.value.single().let {
            assertEquals("highlight", it.type)
            assertNull(it.note)
        }
        assertNull(server.pushedMarks.last().note)
        assertEquals(3, server.pushed.size)

        // The same note again is not a change, and sends nothing.
        reader.updateNote(markA, "")
        advanceUntilIdle()
        assertEquals(3, server.pushed.size)
    }

    /**
     * A phone a minute behind the server, editing a mark the server stamped:
     * the edit still has to be the newer version, or the server keeps the old.
     */
    @Test
    fun `an edit is always later than the version it changes`() = runTest {
        val server = FakeServer()
        server.annotationList = """{"items":[${annotationJson(markA, server.bookFileId, "2026-09-24T20:01:00.5Z")}]}"""
        val reader = opened(server)

        reader.updateNote(markA, "written on a slow clock")
        advanceUntilIdle()
        assertEquals("2026-09-24T20:01:00.501Z", server.pushedMarks.single().updatedAt)
    }

    @Test
    fun `deleting a mark removes it and tells the server`() = runTest {
        val server = FakeServer()
        val reader = opened(server)
        reader.highlight(selection("epub:0/p[1]:0", "epub:0/p[1]:5"))
        advanceUntilIdle()
        reader.markTapped(markA)

        reader.delete(markA)
        advanceUntilIdle()
        assertTrue(reader.marks.value.isEmpty())
        assertNull(reader.openMark.value)
        assertEquals(true, server.pushedMarks.last().deleted)
    }

    @Test
    fun `only a mark this reader holds opens when tapped`() = runTest {
        val server = FakeServer()
        server.annotationList = """{"items":[${annotationJson(markA, server.bookFileId, "2026-09-24T19:00:00Z")}]}"""
        val reader = opened(server)

        reader.markTapped("0b5d1e8a-0000-4000-8000-000000000999")
        reader.markTapped("</script><script>alert(1)")
        assertNull(reader.openMark.value)

        reader.markTapped(markA)
        assertEquals(OpenMark(markA, writing = false), reader.openMark.value)
        reader.closeMark()
        assertNull(reader.openMark.value)
    }

    /** Counted and shown, never acted on: an anchor that stopped resolving is still somebody's note. */
    @Test
    fun `marks the page cannot place are counted, and only marks that exist`() = runTest {
        val server = FakeServer()
        server.annotationList = """{"items":[${annotationJson(markA, server.bookFileId, "2026-09-24T19:00:00Z")}]}"""
        val reader = opened(server)

        reader.unresolved("""["$markA","not-a-mark",7]""", 0)
        assertEquals(1, reader.unplaced.value)
        reader.unresolved("not json", 0)
        reader.unresolved("""{"ids":[]}""", 0)
        assertEquals(1, reader.unplaced.value)
        assertEquals("kept, not dropped", listOf(markA), reader.marks.value.map { it.id })

        // A new chapter starts from nothing until the page says otherwise.
        reader.turnedOffEdge(1)
        advanceUntilIdle()
        assertEquals(0, reader.unplaced.value)
    }

    /**
     * The web's rule, from the phone: own marks in this chapter that the page
     * could not place are flagged, ones flagged before that it now could are
     * cleared - and nothing of anybody's is deleted.
     */
    @Test
    fun `unplaceable marks are reported to the server, and ones placed again are cleared`() = runTest {
        val server = FakeServer()
        val lost = "0b5d1e8a-0000-4000-8000-0000000000b1"
        val back = "0b5d1e8a-0000-4000-8000-0000000000b2"
        val theirs = "0b5d1e8a-0000-4000-8000-0000000000b3"
        val elsewhere = "0b5d1e8a-0000-4000-8000-0000000000b4"
        server.annotationList = """{"items":[
            ${annotationJson(lost, server.bookFileId, "2026-09-24T19:00:00Z")},
            ${annotationJson(back, server.bookFileId, "2026-09-24T19:00:00Z", orphaned = true)},
            ${annotationJson(theirs, server.bookFileId, "2026-09-24T19:00:00Z", shared = true, author = "friend")},
            ${annotationJson(elsewhere, server.bookFileId, "2026-09-24T19:00:00Z", start = "epub:1/p[1]:0", end = "epub:1/p[1]:4", orphaned = true)}
        ]}"""
        val reader = opened(server)

        // A mark made here and not yet sent has no row on the server to flag.
        server.offline = true
        assertTrue(reader.highlight(selection("epub:0/p[1]:0", "epub:0/p[1]:5")))
        advanceUntilIdle()
        server.offline = false

        reader.unresolved("""["$lost","$theirs","$markA"]""", 0)
        advanceUntilIdle()
        val body = kotlinx.serialization.json.Json.parseToJsonElement(server.orphanReports.single()) as kotlinx.serialization.json.JsonObject
        assertEquals("[\"$lost\"]", body["ids"].toString())
        // Placed this time, and only this chapter's: the one in chapter 1 was not looked for.
        assertEquals("[\"$back\"]", body["resolved"].toString())

        val held = reader.marks.value.associateBy { it.id }
        assertTrue(held.getValue(lost).orphaned)
        assertFalse(held.getValue(back).orphaned)
        assertEquals("the flag is not an edit", Delivery.SENT, held.getValue(lost).delivery)
        assertEquals(5, reader.marks.value.size)

        // Said once: the same answer again has nothing new to tell.
        reader.unresolved("""["$lost","$theirs","$markA"]""", 0)
        advanceUntilIdle()
        assertEquals(1, server.orphanReports.size)
    }

    /** Best effort: with no network nothing is queued, and the next paint online says it again. */
    @Test
    fun `an orphan report that cannot be sent changes nothing here`() = runTest {
        val server = FakeServer()
        val lost = "0b5d1e8a-0000-4000-8000-0000000000b1"
        server.annotationList = """{"items":[${annotationJson(lost, server.bookFileId, "2026-09-24T19:00:00Z")}]}"""
        val reader = opened(server)

        server.offline = true
        reader.unresolved("""["$lost"]""", 0)
        advanceUntilIdle()
        assertTrue(server.orphanReports.isEmpty())
        assertFalse(reader.marks.value.single().orphaned)

        server.offline = false
        reader.unresolved("""["$lost"]""", 0)
        advanceUntilIdle()
        assertEquals(1, server.orphanReports.size)
        assertTrue(reader.marks.value.single().orphaned)
    }

    /**
     * The page reports after every paint, empty or not: a chapter where every
     * mark is placed is what clears a flag set earlier.
     */
    @Test
    fun `a chapter where everything is placed clears a flag the server holds`() = runTest {
        val server = FakeServer()
        val back = "0b5d1e8a-0000-4000-8000-0000000000b2"
        server.annotationList = """{"items":[${annotationJson(back, server.bookFileId, "2026-09-24T19:00:00Z", orphaned = true)}]}"""
        val reader = opened(server)

        reader.unresolved("[]", 0)
        advanceUntilIdle()
        val body = kotlinx.serialization.json.Json.parseToJsonElement(server.orphanReports.single()) as kotlinx.serialization.json.JsonObject
        assertEquals("[]", body["ids"].toString())
        assertEquals("[\"$back\"]", body["resolved"].toString())
        assertFalse(reader.marks.value.single().orphaned)
    }

    /**
     * The usual order on opening: the page paints and reports before the
     * marks are back from the server, and does not report again when they
     * come, its list being the same. The flag that came with them is still
     * judged, and cleared.
     */
    @Test
    fun `a flag that arrives after the page reported is still cleared`() = runTest {
        val server = FakeServer()
        val back = "0b5d1e8a-0000-4000-8000-0000000000b2"
        server.annotationList = """{"items":[${annotationJson(back, server.bookFileId, "2026-09-24T19:00:00Z", orphaned = true)}]}"""
        val gate = kotlinx.coroutines.CompletableDeferred<Unit>()
        server.pullGate = gate
        val device = device(tmp.root)
        val reader = ReaderController(
            server.bookFileId, server.client(this), device.kept, device.outbox, device.marks, device.sync, this,
            clock = { now }, newId = { markA },
        )
        // Run, but not advanced in time: the marks' refresh gives up after a
        // few seconds, and held at the gate it would.
        reader.open()
        runCurrent()
        assertTrue("the chapter is not on screen", reader.state.value is ReaderState.Showing)
        assertTrue("the marks came early", reader.marks.value.isEmpty())

        reader.unresolved("[]", 0)
        runCurrent()
        assertTrue(server.orphanReports.isEmpty())

        gate.complete(Unit)
        advanceUntilIdle()
        val body = kotlinx.serialization.json.Json.parseToJsonElement(server.orphanReports.single()) as kotlinx.serialization.json.JsonObject
        assertEquals("[\"$back\"]", body["resolved"].toString())
        assertFalse(reader.marks.value.single().orphaned)
    }

    /**
     * Chapter 0's report arriving once chapter 1 is on screen is about a
     * chapter nobody is looking at. Judged against chapter 1's marks it would
     * clear the flag on a mark the page never tried to place.
     */
    @Test
    fun `a report from a chapter no longer on screen is dropped`() = runTest {
        val server = FakeServer()
        val there = "0b5d1e8a-0000-4000-8000-0000000000b5"
        server.annotationList = """{"items":[${annotationJson(
            there, server.bookFileId, "2026-09-24T19:00:00Z", start = "epub:1/p[1]:0", end = "epub:1/p[1]:4", orphaned = true,
        )}]}"""
        val reader = opened(server)
        reader.turnedOffEdge(1)
        advanceUntilIdle()

        reader.unresolved("[\"$there\"]", 0)
        advanceUntilIdle()
        assertTrue("reported for a chapter not on screen", server.orphanReports.isEmpty())
        assertEquals(0, reader.unplaced.value)
        assertTrue(reader.marks.value.single().orphaned)

        // Chapter 1's own report is heard.
        reader.unresolved("[\"$there\"]", 1)
        advanceUntilIdle()
        assertEquals(1, reader.unplaced.value)
    }

    /** An older page, which does not say which chapter it painted: counted, never reported. */
    @Test
    fun `a report without its chapter is counted but not sent`() = runTest {
        val server = FakeServer()
        server.annotationList = """{"items":[${annotationJson(markA, server.bookFileId, "2026-09-24T19:00:00Z")}]}"""
        val reader = opened(server)
        reader.unresolved("[\"$markA\"]")
        advanceUntilIdle()
        assertEquals(1, reader.unplaced.value)
        assertTrue(server.orphanReports.isEmpty())
    }

    /**
     * A note with no text under it stays a note when cleared, and the server
     * refuses a note with no body. Cleared here, it would look gone on this
     * phone and be refused forever; it is kept, and deleting is the way out.
     */
    @Test
    fun `clearing the only words of a note with no text under it keeps the note`() = runTest {
        val server = FakeServer()
        val textless = annotationJson(markA, server.bookFileId, "2026-09-24T19:00:00Z", note = "a bookmark's thought")
            .replace("\"selectedText\":\"some words\"", "\"selectedText\":null")
        server.annotationList = """{"items":[$textless]}"""
        val reader = opened(server)
        val mark = reader.marks.value.single()
        assertNull(mark.selectedText)
        assertFalse(reader.canClearNote(mark))

        reader.updateNote(markA, "   ")
        advanceUntilIdle()
        assertEquals("a bookmark's thought", reader.marks.value.single().note)
        assertTrue(server.pushed.isEmpty())
    }

    /** Nothing asked for may simply fail to appear: somebody would go on reading believing it was there. */
    @Test
    fun `a highlight that could not be made or kept says so`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        val reader = ReaderController(
            server.bookFileId, server.client(this), device.kept, device.outbox, device.marks, device.sync, this,
            clock = { now }, newId = { markA },
        )
        reader.open()
        advanceUntilIdle()

        assertFalse(reader.highlight("null"))
        assertEquals("That selection could not be highlighted.", reader.notice.value)
        reader.noticeSeen()
        assertNull(reader.notice.value)

        // Signed out underneath the reader: the store is closed to writes.
        device.clear()
        assertTrue(reader.highlight(selection("epub:0/p[1]:0", "epub:0/p[1]:5")))
        advanceUntilIdle()
        assertEquals("That highlight could not be saved.", reader.notice.value)
        assertTrue(reader.marks.value.isEmpty())
    }

    /**
     * Deleted on another device while being written here. The deletion wins,
     * as it does everywhere - but the sheet does not just vanish, and the
     * words typed are handed back, to save again on the same passage.
     */
    @Test
    fun `a note deleted elsewhere while it is being written is said, and its words kept`() = runTest {
        val server = FakeServer()
        server.annotationList = """{"items":[${annotationJson(markA, server.bookFileId, "2026-09-24T19:00:00Z", note = "before")}]}"""
        val device = device(tmp.root)
        val ids = ArrayDeque(listOf("0b5d1e8a-0000-4000-8000-00000000000c"))
        val reader = ReaderController(
            server.bookFileId, server.client(this), device.kept, device.outbox, device.marks, device.sync, this,
            clock = { now }, newId = { ids.removeFirst() },
        )
        reader.open()
        advanceUntilIdle()

        reader.markTapped(markA)
        reader.drafting(markA, "half a paragraph of new thought")

        // The laptop deletes it; the next refresh brings the deletion.
        server.annotationList = """{"items":[]}"""
        server.pulls += pullPage(
            "2026-09-24T21:00:00Z", false,
            deleted = listOf("""{"id":"$markA","bookFileId":"${server.bookFileId}","deletedAt":"2026-09-24T20:30:00Z"}"""),
        )
        reader.open()
        advanceUntilIdle()

        assertNull("the sheet is not left open on nothing", reader.openMark.value)
        assertEquals("This note was deleted on another device.", reader.notice.value)
        val lost = reader.lostNote.value!!
        assertEquals("half a paragraph of new thought", lost.text)
        assertEquals("epub:0/p[2]:0", lost.start)
        assertTrue(reader.marks.value.isEmpty())

        reader.keepLostNote()
        advanceUntilIdle()
        assertNull(reader.lostNote.value)
        reader.marks.value.single().let {
            assertEquals("0b5d1e8a-0000-4000-8000-00000000000c", it.id)
            assertEquals("note", it.type)
            assertEquals("half a paragraph of new thought", it.note)
            assertEquals("epub:0/p[2]:9", it.end)
        }
        assertEquals("a new mark, not the deleted one back", "0b5d1e8a-0000-4000-8000-00000000000c", server.pushedMarks.last().id)
    }

    @Test
    fun `a note deleted elsewhere with nothing typed is said, and nothing is kept`() = runTest {
        val server = FakeServer()
        server.annotationList = """{"items":[${annotationJson(markA, server.bookFileId, "2026-09-24T19:00:00Z", note = "before")}]}"""
        val reader = opened(server)
        reader.markTapped(markA)
        reader.drafting(markA, null)

        server.annotationList = """{"items":[]}"""
        server.pulls += pullPage(
            "2026-09-24T21:00:00Z", false,
            deleted = listOf("""{"id":"$markA","bookFileId":"${server.bookFileId}","deletedAt":"2026-09-24T20:30:00Z"}"""),
        )
        reader.open()
        advanceUntilIdle()

        assertNull(reader.openMark.value)
        assertEquals("This note was deleted on another device.", reader.notice.value)
        assertNull(reader.lostNote.value)
    }

    /** Deleting it here is not "deleted on another device". */
    @Test
    fun `deleting the open mark here says nothing about another device`() = runTest {
        val server = FakeServer()
        server.annotationList = """{"items":[${annotationJson(markA, server.bookFileId, "2026-09-24T19:00:00Z", note = "before")}]}"""
        val reader = opened(server)
        reader.markTapped(markA)
        reader.drafting(markA, "typed")
        reader.delete(markA)
        advanceUntilIdle()
        assertNull(reader.notice.value)
        assertNull(reader.lostNote.value)
    }
}
