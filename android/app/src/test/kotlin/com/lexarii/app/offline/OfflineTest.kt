// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.offline

import com.lexarii.app.api.PushedPosition
import com.lexarii.app.reader.ReaderController
import com.lexarii.app.reader.ReaderState
import com.lexarii.app.reader.Target
import com.lexarii.app.testing.FakeServer
import com.lexarii.app.testing.device
import io.ktor.http.HttpStatusCode
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.time.Instant

/**
 * Reading with no network: a kept copy, a shelf that survives a restart, and
 * positions that survive the app being killed.
 */
class OfflineTest {
    @get:Rule
    val tmp = TemporaryFolder()

    private val png = byteArrayOf(0x89.toByte(), 0x50, 0x4e, 0x47)

    private fun FakeServer.withPictures(): FakeServer = apply {
        assets[assetPath("OEBPS/fig%201.png")] = png
        chapters[1] = """<p>one</p><img src="${assetPath("OEBPS/fig%201.png")}" alt=""><img src="data:image/gif;base64,R0lGOD" alt="">"""
        // The same picture twice is fetched once.
        chapters[2] = """<p>two</p><img alt="" src="${assetPath("OEBPS/fig%201.png")}">"""
    }

    private fun TestScope.reader(server: FakeServer, device: Device, at: String = "2026-09-24T20:00:00Z") =
        ReaderController(server.bookFileId, server.client(this), device.kept, device.outbox, device.marks, device.sync, this, clock = { Instant.parse(at) })

    @Test
    fun `a kept copy opens and pages with the server gone`() = runTest {
        val server = FakeServer().withPictures()
        val device = device(tmp.root)
        device.kept.download(server.client(this), server.bookFileId)
        assertTrue(device.kept.has(server.bookFileId))
        assertEquals(1, server.seen.count { it.url.encodedPath.contains("/assets/") })

        server.offline = true
        val reader = reader(server, device)
        reader.open()
        advanceUntilIdle()
        assertEquals("<p>chapter 0</p>", (reader.state.value as ReaderState.Showing).html)

        reader.turnedOffEdge(1)
        advanceUntilIdle()
        assertTrue((reader.state.value as ReaderState.Showing).html.startsWith("<p>one</p>"))
        assertArrayEquals(png, reader.asset(server.assetPath("OEBPS/fig%201.png")).bytes)
    }

    /**
     * A download that dies halfway is not a copy - and is not thrown away
     * either. The next try asks only for what the first did not get.
     */
    @Test
    fun `a failed download is not kept, and the next one carries on from it`() = runTest {
        val server = FakeServer().withPictures()
        server.broken += "/api/v1/books/${server.bookFileId}/spine/2"
        val device = device(tmp.root)

        try {
            device.kept.download(server.client(this), server.bookFileId)
            fail("a broken chapter did not fail the download")
        } catch (e: Exception) {
            // expected
        }
        assertFalse(device.kept.has(server.bookFileId))
        assertTrue(device.kept.kept().isEmpty())

        server.broken.clear()
        server.seen.clear()
        device.kept.download(server.client(this), server.bookFileId)
        assertTrue(device.kept.has(server.bookFileId))
        val asked = server.seen.map { it.url.encodedPath }
        assertFalse("chapter 0 fetched again", asked.any { it.endsWith("/spine/0") })
        assertFalse("chapter 1 fetched again", asked.any { it.endsWith("/spine/1") })
        assertTrue(asked.any { it.endsWith("/spine/2") })
        assertFalse("its picture fetched again", asked.any { it.contains("/assets/") })
        assertTrue("no partial left once kept", tmp.root.walk().none { it.name.startsWith(".partial") })
    }

    /** Leaving the app stops a download, which then resumes; only discarding it deletes it. */
    @Test
    fun `a cancelled download is kept for resuming, and discarding it leaves nothing behind`() = runTest {
        val server = FakeServer().withPictures()
        val device = device(tmp.root)
        val client = server.client(this)
        // Cancelled from inside, after the first chapter has been written.
        lateinit var job: Job
        job = launch {
            device.kept.download(client, server.bookFileId) { done, _, _ -> if (done == 1L) job.cancel() }
        }
        advanceUntilIdle()
        assertTrue(job.isCancelled)
        assertTrue("the first chapter was fetched", server.seen.any { it.url.encodedPath.endsWith("/spine/0") })
        assertFalse(device.kept.has(server.bookFileId))
        assertTrue("the chapter is held for next time", tmp.root.walk().any { it.isFile && it.name == "0.json" })

        device.kept.remove(server.bookFileId)
        assertTrue(tmp.root.walk().none { it.isFile })
    }

    /** Pieces of the old book would not fit the new one. */
    @Test
    fun `a book that changed since its download began is fetched again from the start`() = runTest {
        val server = FakeServer().withPictures()
        server.broken += "/api/v1/books/${server.bookFileId}/spine/2"
        val device = device(tmp.root)
        runCatching { device.kept.download(server.client(this), server.bookFileId) }

        server.broken.clear()
        server.manifest = """
            {"bookFileId":"${server.bookFileId}","format":"epub",
             "spine":[{"index":0,"linear":true},{"index":1,"linear":true}],"toc":[]}
        """.trimIndent()
        server.chapters[0] = "<p>re-ingested</p>"
        server.seen.clear()
        device.kept.download(server.client(this), server.bookFileId)
        assertTrue(server.seen.any { it.url.encodedPath.endsWith("/spine/0") })
        assertEquals("<p>re-ingested</p>", device.kept.spine(server.bookFileId, 0)?.html)
        assertNull("the old book's third chapter is gone", device.kept.spine(server.bookFileId, 2))
    }

    @Test
    fun `only EPUBs and PDFs are kept`() = runTest {
        val server = FakeServer()
        server.manifest = """{"bookFileId":"${server.bookFileId}","format":"cbz","spine":[],"toc":[]}"""
        val device = device(tmp.root)
        try {
            device.kept.download(server.client(this), server.bookFileId)
            fail("kept a CBZ")
        } catch (e: IllegalArgumentException) {
            // expected
        }
        assertFalse(device.kept.has(server.bookFileId))
        assertTrue(tmp.root.walk().none { it.isFile })
    }

    /**
     * Discarded while its download was finishing: the download's last writes
     * must not put back a copy with no chapters in it, shown as kept.
     */
    @Test
    fun `a copy discarded mid-download stays discarded`() = runTest {
        val server = FakeServer().withPictures()
        val device = device(tmp.root)
        val client = server.client(this)
        var failure: Throwable? = null
        launch {
            try {
                device.kept.download(client, server.bookFileId) { done, _, _ ->
                    if (done == 2L) launch { device.kept.remove(server.bookFileId) }
                }
            } catch (e: Exception) {
                failure = e
            }
        }
        advanceUntilIdle()
        assertEquals("the download was stopped", failure?.message)
        assertFalse(device.kept.has(server.bookFileId))
        assertTrue("something was written after the discard", tmp.root.walk().none { it.isFile })

        // Keeping it again afterwards is a new download, and works.
        device.kept.download(client, server.bookFileId)
        assertTrue(device.kept.has(server.bookFileId))
    }

    /**
     * Signing out while a download runs in the background: it stops writing,
     * and stays stopped even once the next person has signed in.
     */
    @Test
    fun `a download from before a sign-out never writes after it`() = runTest {
        val server = FakeServer().withPictures()
        val device = device(tmp.root)
        val client = server.client(this)
        var failure: Throwable? = null
        launch {
            try {
                device.kept.download(client, server.bookFileId) { done, _, _ ->
                    // Mid-download, the account's copies are cleared - and the
                    // store is open again at once, as the next sign-in opens it.
                    if (done == 1L) launch { device.kept.clear() }
                }
            } catch (e: Exception) {
                failure = e
            }
        }
        advanceUntilIdle()
        assertEquals("signed out", failure?.message)
        assertFalse(device.kept.has(server.bookFileId))
        assertTrue("the old download wrote after the clear", tmp.root.walk().none { it.isFile })

        // A download begun after the clear is nobody's leftover, and works.
        device.kept.download(client, server.bookFileId)
        assertTrue(device.kept.has(server.bookFileId))
    }

    /** The id becomes a directory name, so it has to be one and only one. */
    @Test
    fun `a book file id that is not a plain id is refused`() = runTest {
        val device = device(tmp.root)
        for (bad in listOf("../outbox.json", "a/b", "", ".partial-x")) {
            try {
                device.kept.has(bad)
                fail("accepted $bad")
            } catch (e: IllegalArgumentException) {
                // expected
            }
        }
    }

    @Test
    fun `the images a chapter uses are the ones under its own book file`() {
        val id = "f1"
        val html = """
            <p><img src="/api/v1/books/f1/assets/a.png" alt=""><img alt="x" src="/api/v1/books/f2/assets/b.png">
            <img data-blocked="1" alt=""><img src="data:image/png;base64,AA"><IMG SRC="/api/v1/books/f1/assets/q&amp;a.png">
            <img src="/api/v1/books/f1/assets/a.png"></p>
        """.trimIndent()
        assertEquals(
            listOf("/api/v1/books/f1/assets/a.png", "/api/v1/books/f1/assets/q&a.png"),
            OfflineStore.assetPaths(html, id),
        )
    }

    /** Written before it is sent, so a process killed a moment later has not lost it. */
    @Test
    fun `a position read offline survives the app being killed and goes on the next run`() = runTest {
        val server = FakeServer()
        server.offline = true
        val reader = reader(server, device(tmp.root))
        server.offline = false
        reader.open()
        advanceUntilIdle()

        server.offline = true
        reader.reportPosition("epub:0/p[7]:0", 0.05)
        advanceUntilIdle()
        assertTrue(server.pushed.isEmpty())

        // A new process: nothing in memory, the same files.
        val next = device(tmp.root)
        assertEquals("epub:0/p[7]:0", next.outbox.unsent().single().position)

        server.offline = false
        assertTrue(next.outbox.flush(server.client(this)))
        assertTrue(server.pushed.single().contains("epub:0/p[7]:0"))
        assertTrue(next.outbox.unsent().isEmpty())
        // Sent, but still remembered as where this device was.
        assertEquals("epub:0/p[7]:0", next.outbox.last(server.bookFileId)?.position)
    }

    @Test
    fun `this device's newer position beats the server's older one`() = runTest {
        val server = FakeServer()
        server.progress = HttpStatusCode.OK to
            """{"bookFileId":"${server.bookFileId}","started":true,"locator":"epub:0/p[2]:0","lastReadAt":"2026-09-20T09:00:00Z"}"""
        val device = device(tmp.root)
        device.outbox.put(PushedPosition(server.bookFileId, "epub:2/p[9]:0", 0.8, "2026-09-24T19:00:00Z"))

        val reader = reader(server, device)
        reader.open()
        advanceUntilIdle()

        val showing = reader.state.value as ReaderState.Showing
        assertEquals(2, showing.spineIndex)
        assertEquals(Target.Path("p[9]"), showing.target)
    }

    /** Read further on the laptop since: the laptop wins. */
    @Test
    fun `the server's newer position beats this device's older one`() = runTest {
        val server = FakeServer()
        server.progress = HttpStatusCode.OK to
            """{"bookFileId":"${server.bookFileId}","started":true,"locator":"epub:1/p[4]:0","lastReadAt":"2026-09-24T21:00:00Z"}"""
        val device = device(tmp.root)
        device.outbox.put(PushedPosition(server.bookFileId, "epub:0/p[9]:0", 0.1, "2026-09-24T19:00:00Z"))

        val reader = reader(server, device)
        reader.open()
        advanceUntilIdle()

        val showing = reader.state.value as ReaderState.Showing
        assertEquals(1, showing.spineIndex)
        assertEquals(Target.Path("p[4]"), showing.target)
    }

    @Test
    fun `the shelf from the last run survives a restart`() = runTest {
        val device = device(tmp.root)
        assertNull(device.shelf.load())
        device.shelf.save(ShelfCache.Shelf(emptyList(), "2026-09-24T20:00:00Z"))
        assertEquals("2026-09-24T20:00:00Z", device(tmp.root).shelf.load()?.syncedAt)
    }

    /**
     * Somebody else signing in on this phone - after the last person's token
     * expired, without a sign-out - does not inherit their books or send
     * their reading as their own.
     */
    @Test
    fun `another account signing in clears what the last one left`() = runTest {
        val server = FakeServer().withPictures()
        val device = device(tmp.root)
        assertFalse(device.claim("http://books.local:8080 user-a"))
        device.kept.download(server.client(this), server.bookFileId)
        device.outbox.put(PushedPosition(server.bookFileId, "epub:0/p[1]:0", 0.0, "2026-09-24T19:00:00Z"))

        // The same person again keeps everything.
        assertFalse(device.claim("http://books.local:8080 user-a"))
        assertTrue(device.kept.has(server.bookFileId))

        assertTrue(device.claim("http://books.local:8080 user-b"))
        assertFalse(device.kept.has(server.bookFileId))
        assertTrue(device.outbox.unsent().isEmpty())
        assertNull(device.outbox.last(server.bookFileId))
    }
}
