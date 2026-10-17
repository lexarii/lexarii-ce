// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.offline

import com.lexarii.app.api.PushedPosition
import com.lexarii.app.reader.ReaderController
import com.lexarii.app.testing.FakeServer
import com.lexarii.app.testing.annotationJson
import com.lexarii.app.testing.device
import com.lexarii.app.testing.pullPage
import io.ktor.http.HttpStatusCode
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.StandardTestDispatcher
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.io.File
import java.time.Instant

/**
 * Highlights and notes on the device: made offline, sent once, merged with
 * what changed elsewhere, and never somebody else's to change.
 */
class MarksTest {
    @get:Rule
    val tmp = TemporaryFolder()

    private val markA = "0b5d1e8a-0000-4000-8000-00000000000a"
    private val markB = "0b5d1e8a-0000-4000-8000-00000000000b"
    private val theirs = "0b5d1e8a-0000-4000-8000-0000000000ff"

    private val selection = """{"start":"epub:0/p[2]:4","end":"epub:0/p[2]:19","selectedText":"a passage worth keeping"}"""

    private fun TestScope.reader(server: FakeServer, device: Device, now: () -> Instant = { Instant.parse("2026-09-24T20:00:00Z") }): ReaderController {
        val ids = ArrayDeque(listOf(markA, markB))
        return ReaderController(
            server.bookFileId, server.client(this), device.kept, device.outbox, device.marks, device.sync, this,
            clock = now, newId = { ids.removeFirst() },
        )
    }

    private fun mark(
        id: String,
        bookFileId: String,
        updatedAt: String = "2026-09-24T20:00:00Z",
        note: String? = null,
        start: String = "epub:0/p[2]:0",
        end: String = "epub:0/p[2]:9",
    ) = Mark(
        id = id, bookFileId = bookFileId, type = if (note != null) "note" else "highlight",
        start = start, end = end, selectedText = "some words", note = note,
        createdAt = updatedAt, updatedAt = updatedAt, delivery = Delivery.UNSENT,
    )

    /** Written before it is sent, so a process killed a moment later has not lost it. */
    @Test
    fun `a mark made offline survives a new process and goes with this device's id and time`() = runTest {
        val server = FakeServer()
        val reader = reader(server, device(tmp.root))
        reader.open()
        advanceUntilIdle()

        server.offline = true
        assertTrue(reader.highlight(selection))
        advanceUntilIdle()
        assertTrue(server.pushed.isEmpty())
        assertEquals(markA, reader.marks.value.single().id)

        // A new process: nothing in memory, the same files.
        val next = device(tmp.root)
        val held = next.marks.marks(server.bookFileId).single()
        assertEquals(Delivery.UNSENT, held.delivery)
        assertEquals(1, next.marks.unsent())

        server.offline = false
        assertTrue(next.marks.flush(server.client(this)))
        val sent = server.pushedMarks.single()
        assertEquals(markA, sent.id)
        assertEquals(server.bookFileId, sent.bookFileId)
        assertEquals("2026-09-24T20:00:00Z", sent.updatedAt)
        assertEquals("highlight", sent.type)
        assertEquals("epub:0/p[2]:4", sent.start)
        assertEquals("epub:0/p[2]:19", sent.end)
        assertEquals("a passage worth keeping", sent.selectedText)
        assertNull(sent.deleted)
        assertEquals(Delivery.SENT, next.marks.marks(server.bookFileId).single().delivery)
        assertEquals(0, next.marks.unsent())
    }

    /** The connection that drops is the one carrying the response. */
    @Test
    fun `pushing the same mark twice is harmless`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        val client = server.client(this)
        device.marks.put(mark(markA, server.bookFileId))

        server.losePushResponses = 1
        assertFalse(device.marks.flush(client))
        assertEquals(Delivery.UNSENT, device.marks.marks(server.bookFileId).single().delivery)

        // What the server says to a version it already holds - and it lists
        // that version, which a stale answer now fetches.
        server.markStatus[markA] = "stale"
        server.annotationList = """{"items":[${annotationJson(markA, server.bookFileId, "2026-09-24T20:00:00Z")}]}"""
        assertTrue(device.marks.flush(client))

        val both = server.pushedMarks
        assertEquals(2, both.size)
        assertEquals("the same mark, not a second one", both[0], both[1])
        assertEquals(Delivery.SENT, device.marks.marks(server.bookFileId).single().delivery)

        // And nothing further to send.
        assertTrue(device.marks.flush(client))
        assertEquals(2, server.pushed.size)
    }

    @Test
    fun `a mark the server refuses is kept and not sent again until it changes`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        val client = server.client(this)
        device.marks.put(mark(markA, server.bookFileId))
        server.markStatus[markA] = "refused"

        assertTrue("nothing left to send", device.marks.flush(client))
        assertEquals(Delivery.REFUSED, device.marks.marks(server.bookFileId).single().delivery)
        device.marks.flush(client)
        assertEquals(1, server.pushed.size)

        // Changed, it is somebody's new word on it, and goes again.
        device.marks.put(mark(markA, server.bookFileId, updatedAt = "2026-09-24T21:00:00Z", note = "again"))
        device.marks.flush(client)
        assertEquals(2, server.pushed.size)
    }

    @Test
    fun `a deletion is sent as one and then forgotten`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        device.marks.put(mark(markA, server.bookFileId))
        assertTrue(device.marks.delete(server.bookFileId, markA, "2026-09-24T21:00:00Z"))
        assertTrue(device.marks.marks(server.bookFileId).isEmpty())
        assertEquals("the deletion is still to send", 1, device.marks.unsent())

        assertTrue(device.marks.flush(server.client(this)))
        val sent = server.pushedMarks.single()
        assertEquals(markA, sent.id)
        assertEquals(true, sent.deleted)
        assertEquals("2026-09-24T21:00:00Z", sent.updatedAt)
        assertEquals(0, device.marks.unsent())
        assertTrue("nothing left on disk for this book", File(tmp.root, "account/marks").listFiles().orEmpty().isEmpty())
    }

    private fun TestScope.sync(device: Device, pageSize: Int) =
        Sync(File(tmp.root, "account/sync.json"), device.marks, device.outbox, StandardTestDispatcher(testScheduler), pageSize = pageSize)

    private fun ids(n: Int) = (1..n).map { "0b5d1e8a-0000-4000-8000-%012d".format(it) }

    /**
     * A burst of changes stamped with one instant - an import - is exactly
     * what paging by time could not get past. Paged by the server's cursor,
     * every one arrives, once.
     */
    @Test
    fun `a round pages by the server's cursor, with no gaps or repeats`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        val all = ids(7)
        server.syncRows = all.map { annotationJson(it, server.bookFileId, "2026-09-24T10:00:00Z") }
        server.until = "2026-09-24T21:00:00Z"

        assertEquals("2026-09-24T21:00:00Z", sync(device, pageSize = 3).pull(server.client(this)))

        assertEquals(all.toSet(), device.marks.marks(server.bookFileId).map { it.id }.toSet())
        assertEquals(7, device.marks.marks(server.bookFileId).size)
        // The first call from the stored `since` (none yet), each after it by
        // the cursor it was handed, verbatim, and nothing else.
        assertEquals(listOf(null, "after-3", "after-6"), server.pulledCursor)
        assertEquals(listOf<String?>(null, null, null), server.pulledSince)
        assertEquals("2026-09-24T21:00:00Z", device.sync.since())

        // The next round starts from that `until`, with no cursor.
        server.syncRows = emptyList()
        sync(device, pageSize = 3).pull(server.client(this))
        assertEquals("2026-09-24T21:00:00Z", server.pulledSince.last())
        assertNull(server.pulledCursor.last())
    }

    /** Storing an earlier page's `until` would skip everything that did not fit on it. */
    @Test
    fun `a round cut off midway leaves the stored since, and the next starts over from it`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        val sync = sync(device, pageSize = 2)
        server.syncRows = emptyList()
        server.until = "2026-09-24T09:00:00Z"
        sync.pull(server.client(this))

        server.syncRows = ids(5).map { annotationJson(it, server.bookFileId, "2026-09-24T10:00:00Z") }
        server.until = "2026-09-24T21:00:00Z"
        server.failPullAt = "after-4"
        try {
            sync.pull(server.client(this))
            fail("a round cut off halfway reported success")
        } catch (e: Exception) {
            // expected
        }
        assertEquals("the cursor must not move past what did not arrive", "2026-09-24T09:00:00Z", sync.since())
        assertEquals("what did arrive is kept", 4, device.marks.marks(server.bookFileId).size)

        server.failPullAt = null
        assertEquals("2026-09-24T21:00:00Z", sync.pull(server.client(this)))
        assertEquals(5, device.marks.marks(server.bookFileId).size)
        // Started over from the stored `since`, not from the dead round's cursor.
        assertEquals(listOf(null, "after-2", "after-4", null, "after-2", "after-4"), server.pulledCursor.drop(1))
        assertEquals("2026-09-24T09:00:00Z", server.pulledSince[4])
    }

    /** A safety net: a server fault must not keep a phone asking. */
    @Test
    fun `a page that says more with no new cursor ends the round unfinished`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        val row = listOf(annotationJson(markA, server.bookFileId, "2026-09-24T10:00:00Z"))
        server.pulls += pullPage("2026-09-24T21:00:00Z", more = true, annotations = row, cursor = "same")
        server.pulls += pullPage("2026-09-24T21:00:00Z", more = true, annotations = row, cursor = "same")
        server.pulls += pullPage("2026-09-24T21:00:00Z", more = true, annotations = row)

        assertNull(device.sync.pull(server.client(this)))
        assertEquals(2, server.pulledCursor.size)
        assertNull(device.sync.since())

        assertNull(device.sync.pull(server.client(this)))
        assertEquals("more, but no cursor to ask with", 3, server.pulledCursor.size)
        assertNull(device.sync.since())
    }

    /**
     * A pull never decides against an edit not yet sent. The row time a pull
     * carries is when the server stored a version, not when it was made, so
     * it cannot say which was made last; the push can, by the rule the server
     * applies to it.
     */
    @Test
    fun `a pull never replaces an unsent edit here, however new it looks`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        val client = server.client(this)
        device.marks.put(mark(markA, server.bookFileId, updatedAt = "2026-09-24T20:00:00Z", note = "mine"))

        for ((until, at) in listOf("2026-09-24T21:00:00Z" to "2026-09-24T19:00:00Z", "2026-09-24T22:00:00Z" to "2026-09-24T21:30:00Z")) {
            server.pulls += pullPage(until, false, listOf(annotationJson(markA, server.bookFileId, at, note = "from elsewhere")))
            device.sync.pull(client)
            device.marks.marks(server.bookFileId).single().let {
                assertEquals("mine", it.note)
                assertEquals(Delivery.UNSENT, it.delivery)
            }
        }
        assertEquals("2026-09-24T22:00:00Z", device.sync.since())
    }

    /**
     * Stale: the server kept a version made later. The row did not change, so
     * no pull will ever bring it - the push fetches it, and a mark the server
     * no longer lists at all was deleted elsewhere.
     */
    @Test
    fun `a stale push fetches the version that won`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        device.marks.put(mark(markA, server.bookFileId, updatedAt = "2026-09-24T20:00:00Z", note = "lost"))
        device.marks.put(mark(markB, server.bookFileId, updatedAt = "2026-09-24T20:00:00Z", note = "also lost"))
        server.markStatus[markA] = "stale"
        server.markStatus[markB] = "stale"
        // Stored by the server before this phone's push, and still the winner.
        server.annotationList = """{"items":[${annotationJson(markA, server.bookFileId, "2026-09-24T19:00:00Z", note = "won")}]}"""

        assertTrue(device.marks.flush(server.client(this)))

        val held = device.marks.marks(server.bookFileId)
        assertEquals(listOf(markA), held.map { it.id })
        assertEquals("won", held.single().note)
        assertEquals(Delivery.SENT, held.single().delivery)
    }

    /** A list read a moment before this phone's last push landed must not put back what it replaced. */
    @Test
    fun `an older version from the server does not replace a newer one sent from here`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        device.marks.put(mark(markA, server.bookFileId, updatedAt = "2026-09-24T20:00:00Z", note = "sent from here"))
        device.marks.flush(server.client(this))

        device.marks.applyListed(server.bookFileId, listOf(annotation(markA, server.bookFileId, "2026-09-24T19:00:00Z", "before")))
        assertEquals("sent from here", device.marks.marks(server.bookFileId).single().note)

        device.marks.applyListed(server.bookFileId, listOf(annotation(markA, server.bookFileId, "2026-09-24T21:00:00Z", "after")))
        assertEquals("after", device.marks.marks(server.bookFileId).single().note)
    }

    /**
     * An edit made at 19:00 on a phone that was offline arrives at 22:00. The
     * row time says it is the newest thing there; the edit time says it is not.
     */
    @Test
    fun `merging compares when a version was made, not when the server stored it`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        device.marks.put(mark(markA, server.bookFileId, updatedAt = "2026-09-24T20:00:00Z", note = "made at eight"))
        device.marks.flush(server.client(this))

        device.marks.applyListed(
            server.bookFileId,
            listOf(annotation(markA, server.bookFileId, "2026-09-24T22:00:00Z", "made at seven", editedAt = "2026-09-24T19:00:00Z")),
        )
        device.marks.marks(server.bookFileId).single().let {
            assertEquals("made at eight", it.note)
            assertEquals("2026-09-24T20:00:00Z", it.updatedAt)
        }

        device.marks.applyListed(
            server.bookFileId,
            listOf(annotation(markA, server.bookFileId, "2026-09-24T20:30:00Z", "made at nine", editedAt = "2026-09-24T21:00:00Z")),
        )
        device.marks.marks(server.bookFileId).single().let {
            assertEquals("made at nine", it.note)
            assertEquals("the edit time is what is kept, for the next comparison", "2026-09-24T21:00:00Z", it.updatedAt)
        }
    }

    /** Marked sent only if still the version that went. */
    @Test
    fun `an edit made while its push is in the air stays unsent`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        val client = server.client(this)
        device.marks.put(mark(markA, server.bookFileId, updatedAt = "2026-09-24T20:00:00Z", note = "first"))

        val gate = CompletableDeferred<Unit>()
        server.pushGate = gate
        val flushing = launch { device.marks.flush(client) }
        advanceUntilIdle()
        device.marks.put(mark(markA, server.bookFileId, updatedAt = "2026-09-24T20:01:00Z", note = "second"))
        gate.complete(Unit)
        flushing.join()

        device.marks.marks(server.bookFileId).single().let {
            assertEquals("second", it.note)
            assertEquals(Delivery.UNSENT, it.delivery)
        }
        server.pushGate = null
        assertTrue(device.marks.flush(client))
        assertEquals("second", server.pushedMarks.last().note)
    }

    /**
     * A deletion beats an edit, on the server and so here: the server would
     * refuse to bring the mark back, and a mark kept here would be one no
     * other device ever sees.
     */
    @Test
    fun `a deletion from elsewhere removes the mark, even one edited here`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        val client = server.client(this)
        server.pulls += pullPage("2026-09-24T21:00:00Z", false, listOf(annotationJson(markA, server.bookFileId, "2026-09-24T19:00:00Z")))
        device.sync.pull(client)
        device.marks.put(mark(markB, server.bookFileId, updatedAt = "2026-09-24T23:00:00Z", note = "edited here"))
        assertEquals(2, device.marks.marks(server.bookFileId).size)

        val gone = listOf(markA, markB).map { """{"id":"$it","bookFileId":"${server.bookFileId}","deletedAt":"2026-09-24T21:10:00Z"}""" }
        server.pulls += pullPage("2026-09-24T22:00:00Z", false, deleted = gone)
        device.sync.pull(client)

        assertTrue(device.marks.marks(server.bookFileId).isEmpty())
        assertEquals(0, device.marks.unsent())
    }

    /** A mark deleted here and not yet sent does not come back with the next pull. */
    @Test
    fun `a mark deleted here is not brought back by a pull`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        server.offline = true
        device.marks.put(mark(markA, server.bookFileId))
        device.marks.delete(server.bookFileId, markA, "2026-09-24T21:00:00Z")
        server.offline = false
        server.pulls += pullPage("2026-09-24T22:00:00Z", false, listOf(annotationJson(markA, server.bookFileId, "2026-09-24T21:30:00Z")))
        device.sync.pull(server.client(this))
        assertTrue(device.marks.marks(server.bookFileId).isEmpty())
    }

    @Test
    fun `shared marks are shown but never sent, edited, or deleted`() = runTest {
        val server = FakeServer()
        server.annotationList = """{"items":[
            ${annotationJson(markA, server.bookFileId, "2026-09-24T19:00:00Z")},
            ${annotationJson(theirs, server.bookFileId, "2026-09-24T19:00:00Z", note = "I disagree", shared = true, author = "sam")}
        ]}"""
        val device = device(tmp.root)
        val reader = reader(server, device)
        reader.open()
        advanceUntilIdle()

        val shared = reader.marks.value.single { it.id == theirs }
        assertTrue(shared.shared)
        assertEquals("sam", shared.author)

        reader.updateNote(theirs, "no you don't")
        reader.delete(theirs)
        advanceUntilIdle()
        assertEquals("I disagree", reader.marks.value.single { it.id == theirs }.note)
        assertFalse(device.marks.put(shared.copy(note = "forced")))
        assertFalse(device.marks.delete(server.bookFileId, theirs, "2026-09-24T21:00:00Z"))
        assertTrue(device.marks.flush(server.client(this)))
        assertTrue(server.pushedMarks.none { it.id == theirs })
        assertEquals(0, device.marks.unsent())

        // Withdrawn: the next list does not have it, and neither does the reader.
        server.annotationList = """{"items":[${annotationJson(markA, server.bookFileId, "2026-09-24T19:00:00Z")}]}"""
        reader.open()
        advanceUntilIdle()
        assertEquals(listOf(markA), reader.marks.value.map { it.id })
    }

    /** Somebody else signing in on this phone does not inherit the last person's notes, or their cursor. */
    @Test
    fun `another account signing in clears the marks and the sync cursor`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        device.claim("http://books.local:8080 user-a")
        device.marks.put(mark(markA, server.bookFileId))
        device.sync.pull(server.client(this))
        assertEquals(server.until, device.sync.since())

        assertTrue(device.claim("http://books.local:8080 user-b"))
        assertTrue(device.marks.marks(server.bookFileId).isEmpty())
        assertEquals(0, device.marks.unsent())
        assertNull(device.sync.since())
        // And not from a fresh process either.
        assertTrue(device(tmp.root).marks.marks(server.bookFileId).isEmpty())
    }

    @Test
    fun `a newer position from elsewhere is remembered, but never over one not yet sent`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        val other = "6f1c7a52-0000-4000-8000-000000000002"
        device.outbox.put(PushedPosition(server.bookFileId, "epub:5/p[1]:0", 0.9, "2026-09-24T19:00:00Z"))
        device.outbox.put(PushedPosition(other, "epub:1/p[1]:0", 0.1, "2026-09-24T19:00:00Z"))
        device.outbox.flush(server.client(this))
        // Read further on this phone since, not yet sent.
        device.outbox.put(PushedPosition(server.bookFileId, "epub:6/p[1]:0", 0.95, "2026-09-24T19:30:00Z"))

        fun position(id: String, at: String) =
            """{"bookFileId":"$id","position":"epub:0/p[3]:0","furthest":"epub:0/p[3]:0","readAt":"$at","updatedAt":"$at"}"""
        server.pulls += pullPage(
            "2026-09-24T21:00:00Z", false,
            positions = listOf(position(server.bookFileId, "2026-09-24T20:00:00Z"), position(other, "2026-09-24T20:00:00Z")),
        )
        device.sync.pull(server.client(this))

        assertEquals("epub:6/p[1]:0", device.outbox.last(server.bookFileId)?.position)
        assertEquals("epub:0/p[3]:0", device.outbox.last(other)?.position)
        assertEquals("the learned one is not sent back", 1, device.outbox.unsent().size)
    }

    /**
     * Signing out while a pull is still coming back over a slow network: what
     * arrives after the clear lands nowhere, and the next account starts from
     * an empty device.
     */
    @Test
    fun `a pull that comes back after sign-out leaves nothing behind`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        device.claim("http://books.local:8080 user-a")
        server.pulls += pullPage(
            "2026-09-24T21:00:00Z", false,
            annotations = listOf(annotationJson(markA, server.bookFileId, "2026-09-24T19:00:00Z", note = "a's note")),
            positions = listOf("""{"bookFileId":"${server.bookFileId}","position":"epub:0/p[3]:0","furthest":"epub:0/p[3]:0","readAt":"2026-09-24T20:00:00Z","updatedAt":"2026-09-24T20:00:00Z"}"""),
        )
        val gate = CompletableDeferred<Unit>()
        server.pullGate = gate
        val pulling = launch { runCatching { device.sync.pull(server.client(this@runTest)) } }
        advanceUntilIdle()

        device.clear()
        gate.complete(Unit)
        pulling.join()

        assertEquals(listOf<String>(), tmp.root.walk().filter { it.isFile }.map { it.path }.toList())
        assertFalse("user b inherits nothing", device.claim("http://books.local:8080 user-b"))
        assertTrue(device.marks.marks(server.bookFileId).isEmpty())
        assertNull(device.sync.since())
        assertNull(device.outbox.last(server.bookFileId))
        // And the stores work again for the account now signed in.
        assertTrue(device.marks.put(mark(markB, server.bookFileId)))
    }

    /** Account data with nobody's name on it is nobody this account can vouch for. */
    @Test
    fun `account data with no owner is cleared by the next claim`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        device.claim("http://books.local:8080 user-a")
        device.marks.put(mark(markA, server.bookFileId))
        assertTrue(File(tmp.root, "account/owner").delete())

        val next = device(tmp.root)
        assertTrue(next.claim("http://books.local:8080 user-b"))
        assertTrue(next.marks.marks(server.bookFileId).isEmpty())
    }

    @Test
    fun `refused marks are counted apart from unsent ones`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        device.marks.put(mark(markA, server.bookFileId))
        device.marks.put(mark(markB, server.bookFileId))
        server.markStatus[markA] = "refused"
        device.marks.flush(server.client(this))
        device.marks.put(mark(theirs, server.bookFileId))

        assertEquals(1, device.marks.refused())
        assertEquals(1, device.marks.unsent())
    }

    /** Skipped, the next mark made in that book would write over the file and the notes in it. */
    @Test
    fun `a marks file that cannot be read is moved aside, not overwritten`() = runTest {
        val server = FakeServer()
        val dir = File(tmp.root, "account/marks").apply { mkdirs() }
        val broken = File(dir, hashed(server.bookFileId) + ".json").apply { writeText("{\"bookFileId\": \"truncated") }

        val device = device(tmp.root)
        assertTrue(device.marks.put(mark(markA, server.bookFileId)))

        val aside = dir.listFiles().orEmpty().single { it.name.startsWith(broken.name + ".corrupt-") }
        assertEquals("{\"bookFileId\": \"truncated", aside.readText())
        assertEquals(listOf(markA), device(tmp.root).marks.marks(server.bookFileId).map { it.id })
    }

    /**
     * The reviewer's probe. The version that won a stale push replaces the one
     * that lost - never an edit made while that push was in the air, which is
     * newer than both and has not been sent.
     */
    @Test
    fun `a stale answer does not replace an edit made while its push was in the air`() = runTest {
        val server = FakeServer()
        val device = device(tmp.root)
        val client = server.client(this)
        device.marks.put(mark(markA, server.bookFileId, updatedAt = "2026-09-24T20:00:00Z", note = "first"))
        server.markStatus[markA] = "stale"
        server.annotationList = """{"items":[${annotationJson(markA, server.bookFileId, "2026-09-24T20:00:30Z", note = "web", editedAt = "2026-09-24T20:00:30Z")}]}"""

        val gate = CompletableDeferred<Unit>()
        server.pushGate = gate
        val flushing = launch { device.marks.flush(client) }
        advanceUntilIdle()
        device.marks.put(mark(markA, server.bookFileId, updatedAt = "2026-09-24T20:01:00Z", note = "second, newest"))
        gate.complete(Unit)
        flushing.join()

        device.marks.marks(server.bookFileId).single().let {
            assertEquals("second, newest", it.note)
            assertEquals(Delivery.UNSENT, it.delivery)
        }
        server.pushGate = null
        server.markStatus[markA] = "applied"
        assertTrue(device.marks.flush(client))
        assertEquals("second, newest", server.pushedMarks.last().note)
        assertEquals("second, newest", device.marks.marks(server.bookFileId).single().note)
    }

    private fun annotation(id: String, bookFileId: String, updatedAt: String, note: String, editedAt: String = updatedAt) =
        kotlinx.serialization.json.Json { ignoreUnknownKeys = true }
            .decodeFromString<com.lexarii.app.api.Annotation>(annotationJson(id, bookFileId, updatedAt, note = note, editedAt = editedAt))
}
