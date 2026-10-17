// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import com.lexarii.app.testing.FakeServer
import com.lexarii.app.testing.device
import io.ktor.http.HttpStatusCode
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.time.Instant

/**
 * The reader's decisions, against a server that is not there: which chapter
 * to open, where, and what gets sent back as this person's place.
 */
class ReaderControllerTest {
    @get:Rule
    val tmp = TemporaryFolder()

    private val id = "6f1c7a52-0000-4000-8000-000000000001"

    private val manifest = """
        {"bookFileId":"$id","format":"epub",
         "spine":[{"index":0,"linear":true},{"index":1,"linear":true},{"index":2,"linear":true}],
         "toc":[]}
    """.trimIndent()

    private fun Server(manifest: String, progress: Pair<HttpStatusCode, String>, offline: Boolean = false) =
        FakeServer(id).also {
            it.manifest = manifest
            it.progress = progress
            it.offline = offline
        }

    private fun TestScope.controller(server: FakeServer): ReaderController {
        val device = device(tmp.root)
        return ReaderController(id, server.client(this), device.kept, device.outbox, device.marks, device.sync, this, clock = { Instant.parse("2026-09-24T20:00:00Z") })
    }

    private val unread = HttpStatusCode.OK to """{"bookFileId":"$id","started":false}"""

    @Test
    fun `opens where it was left, on every device`() = runTest {
        val server = Server(manifest, HttpStatusCode.OK to """{"bookFileId":"$id","started":true,"locator":"epub:2/div[1]/p[5]:0"}""")
        val reader = controller(server)

        reader.open()
        advanceUntilIdle()

        val showing = reader.state.value as ReaderState.Showing
        assertEquals(2, showing.spineIndex)
        assertEquals(3, showing.spineCount)
        assertEquals(Target.Path("div[1]/p[5]"), showing.target)
        assertEquals("<p>chapter 2</p>", showing.html)
    }

    @Test
    fun `a book never opened starts at the beginning`() = runTest {
        val reader = controller(Server(manifest, unread))
        reader.open()
        advanceUntilIdle()

        val showing = reader.state.value as ReaderState.Showing
        assertEquals(0, showing.spineIndex)
        assertEquals(Target.Start, showing.target)
    }

    /** A saved place is a convenience. Failing to fetch it must not stop anybody reading. */
    @Test
    fun `a position that cannot be read or no longer fits opens at the beginning`() = runTest {
        for (progress in listOf(
            HttpStatusCode.InternalServerError to """{"error":{"code":"internal","message":"boom"}}""",
            HttpStatusCode.OK to """{"bookFileId":"$id","started":true,"locator":"epub:9/p[1]:0"}""",
            HttpStatusCode.OK to """{"bookFileId":"$id","started":true,"locator":"pdf:3:0"}""",
        )) {
            val reader = controller(Server(manifest, progress))
            reader.open()
            advanceUntilIdle()
            val showing = reader.state.value as ReaderState.Showing
            assertEquals(progress.second, 0, showing.spineIndex)
        }
    }

    @Test
    fun `a PDF says so instead of showing a blank page`() = runTest {
        val reader = controller(Server("""{"bookFileId":"$id","format":"pdf","spine":[],"toc":[]}""", unread))
        reader.open()
        advanceUntilIdle()
        assertEquals(ReaderState.Unsupported("pdf"), reader.state.value)
    }

    @Test
    fun `running off a chapter opens the next at its start and the previous at its end`() = runTest {
        val server = Server(manifest, HttpStatusCode.OK to """{"bookFileId":"$id","started":true,"locator":"epub:1/p[1]:0"}""")
        val reader = controller(server)
        reader.open()
        advanceUntilIdle()

        reader.turnedOffEdge(1)
        advanceUntilIdle()
        (reader.state.value as ReaderState.Showing).let {
            assertEquals(2, it.spineIndex)
            assertEquals(Target.Start, it.target)
        }

        reader.turnedOffEdge(-1)
        advanceUntilIdle()
        (reader.state.value as ReaderState.Showing).let {
            assertEquals(1, it.spineIndex)
            assertEquals(Target.End, it.target)
        }
    }

    @Test
    fun `past either end of the book nothing happens`() = runTest {
        val server = Server(manifest, unread)
        val reader = controller(server)
        reader.open()
        advanceUntilIdle()
        val before = reader.state.value
        val requests = server.seen.size

        reader.turnedOffEdge(-1)
        reader.follow(7, "")
        advanceUntilIdle()

        assertEquals(before, reader.state.value)
        assertEquals("no request for a chapter that does not exist", requests, server.seen.size)
    }

    @Test
    fun `a link into another chapter lands on its anchor`() = runTest {
        val reader = controller(Server(manifest, unread))
        reader.open()
        advanceUntilIdle()

        reader.follow(2, "note-4")
        advanceUntilIdle()

        val showing = reader.state.value as ReaderState.Showing
        assertEquals(2, showing.spineIndex)
        assertEquals(Target.Fragment("note-4"), showing.target)
    }

    /**
     * The sync push, with this device's own clock on it: a position recorded
     * offline and sent an hour later must land as an hour old.
     */
    @Test
    fun `the position goes to the server through the sync push`() = runTest {
        val server = Server(manifest, unread)
        val reader = controller(server)
        reader.open()
        advanceUntilIdle()

        reader.reportPosition("epub:0/p[4]:0", 0.12)
        advanceUntilIdle()

        val body = server.pushed.single()
        assertTrue(body, body.contains("\"bookFileId\":\"$id\""))
        assertTrue(body, body.contains("\"position\":\"epub:0/p[4]:0\""))
        assertTrue(body, body.contains("\"readAt\":\"2026-09-24T20:00:00Z\""))
        assertTrue(body, body.contains("\"percent\":0.12"))
    }

    /** It came out of a WebView holding untrusted HTML. */
    @Test
    fun `a position that is not a place in the chapter on screen is not sent`() = runTest {
        val server = Server(manifest, unread)
        val reader = controller(server)
        reader.open()
        advanceUntilIdle()

        reader.reportPosition("epub:2/p[4]:0", 0.9)
        reader.reportPosition("javascript:alert(1)", 0.1)
        reader.reportPosition("epub:0/a:b:0", 0.1)
        advanceUntilIdle()

        assertTrue(server.pushed.toString(), server.pushed.isEmpty())
    }

    @Test
    fun `a position that could not be sent is sent when the reader closes`() = runTest {
        val server = Server(manifest, unread)
        val reader = controller(server)
        reader.open()
        advanceUntilIdle()

        server.offline = true
        reader.reportPosition("epub:0/p[9]:0", 0.2)
        advanceUntilIdle()
        assertTrue(server.pushed.isEmpty())

        server.offline = false
        reader.send()
        assertTrue(server.pushed.single().contains("epub:0/p[9]:0"))

        // And only once.
        reader.send()
        assertEquals(1, server.pushed.size)
    }

    /** A rotation throws the WebView away; the new one opens on the paragraph last reported. */
    @Test
    fun `a new WebView goes back to the paragraph last reported`() = runTest {
        val reader = controller(Server(manifest, unread))
        reader.open()
        advanceUntilIdle()
        val first = reader.state.value as ReaderState.Showing

        reader.reportPosition("epub:0/div[2]/p[3]:0", 0.1)
        advanceUntilIdle()
        reader.reattached()

        val again = reader.state.value as ReaderState.Showing
        assertEquals(Target.Path("div[2]/p[3]"), again.target)
        assertTrue("the same chapter shown again must still be a change", again.generation > first.generation)
    }

    @Test
    fun `an unreachable server is an error with a way to try again`() = runTest {
        val server = Server(manifest, unread, offline = true)
        val reader = controller(server)
        reader.open()
        advanceUntilIdle()
        assertTrue(reader.state.value is ReaderState.Failed)

        server.offline = false
        reader.retry()
        advanceUntilIdle()
        assertTrue(reader.state.value is ReaderState.Showing)
    }
}
