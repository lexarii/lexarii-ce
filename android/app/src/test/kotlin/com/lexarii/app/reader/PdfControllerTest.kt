// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import com.lexarii.app.api.PushedPosition
import com.lexarii.app.offline.Device
import com.lexarii.app.testing.FakeServer
import com.lexarii.app.testing.device
import io.ktor.http.HttpStatusCode
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.time.Instant

/**
 * The PDF reader's decisions: which page, from where, what is fetched ahead
 * and how it is marked, and what gets sent back as this person's place.
 */
class PdfControllerTest {
    @get:Rule
    val tmp = TemporaryFolder()

    private fun page(n: Int) = "%PDF-1.7 page $n".toByteArray()

    private fun pdfServer(pages: Int = 5, canDownload: Boolean = false, unitCount: Int? = pages) = FakeServer().apply {
        manifest = """{"bookFileId":"$bookFileId","format":"pdf","spine":[],"toc":[{"title":"Two","spineIndex":2},{"title":"Lost","spineIndex":-1}]}"""
        info = """{"bookFileId":"$bookFileId","title":"A PDF","workId":"w1","format":"pdf","sizeBytes":100,"status":"ready",""" +
            (unitCount?.let { "\"unitCount\":$it," } ?: "") + """"reflowable":false,"canDownload":$canDownload}"""
        for (n in 1..pages) this.pages[n] = page(n)
        progress = HttpStatusCode.OK to """{"bookFileId":"$bookFileId","started":false}"""
    }

    private fun TestScope.controller(server: FakeServer, device: Device, at: String = "2026-09-25T10:00:00Z") =
        PdfController(server.bookFileId, server.client(this), device.kept, device.pages, device.outbox, this, clock = { Instant.parse(at) })

    private fun pageRequests(server: FakeServer) = server.seen
        .filter { it.url.encodedPath.contains("/page/") }
        .map { it.url.encodedPath.substringAfterLast('/').toInt() to (it.url.parameters["prefetch"] == "1") }

    @Test
    fun `opens where it was left, a page at a time, with its neighbours fetched as prefetches`() = runTest {
        val server = pdfServer()
        server.progress = HttpStatusCode.OK to
            """{"bookFileId":"${server.bookFileId}","started":true,"locator":"pdf:3:0","lastReadAt":"2026-09-24T10:00:00Z"}"""
        val device = device(tmp.root)
        val reader = controller(server, device)
        reader.open()
        advanceUntilIdle()

        val showing = reader.state.value as PdfState.Showing
        assertEquals(3, showing.page)
        assertEquals(5, showing.pageCount)
        assertEquals("a one-page document", 0, showing.index)
        assertArrayEquals(page(3), showing.file.readBytes())
        // The page looked at is a read; the ones either side are not.
        assertEquals(listOf(3 to false, 4 to true, 2 to true), pageRequests(server))
        assertEquals("Two", reader.toc.value.first().title)
    }

    @Test
    fun `turning onto a prefetched page asks for nothing, and the place is sent as a locator`() = runTest {
        val server = pdfServer()
        val device = device(tmp.root)
        val reader = controller(server, device)
        reader.open()
        advanceUntilIdle()
        assertEquals(1, (reader.state.value as PdfState.Showing).page)

        server.seen.clear()
        reader.turn(1)
        advanceUntilIdle()
        assertEquals(2, (reader.state.value as PdfState.Showing).page)
        assertTrue("page 2 was asked for again", pageRequests(server).none { it.first == 2 })
        assertEquals(listOf(3 to true), pageRequests(server))

        val sent = server.pushed.last()
        assertTrue(sent, sent.contains("\"position\":\"pdf:2:0\""))
        assertTrue(sent, sent.contains("\"percent\":0.4"))
        assertTrue(sent, sent.contains("2026-09-25T10:00:00Z"))
    }

    @Test
    fun `pages far from the one being read are let go`() = runTest {
        val server = pdfServer(pages = 12)
        val device = device(tmp.root)
        val reader = controller(server, device)
        reader.open()
        advanceUntilIdle()
        repeat(7) {
            reader.turn(1)
            advanceUntilIdle()
        }
        assertEquals(8, (reader.state.value as PdfState.Showing).page)
        assertNull(device.pages.get(server.bookFileId, 1))
        assertNull(device.pages.get(server.bookFileId, 5))
        assertNotNull(device.pages.get(server.bookFileId, 7))
        assertNotNull(device.pages.get(server.bookFileId, 9))

        // Closing the book lets go of the rest.
        reader.send()
        assertNull(device.pages.get(server.bookFileId, 8))
    }

    /** A jump from the contents while the pages around the old place are being pruned. */
    @Test
    fun `a jump far away keeps the page it jumped to`() = runTest {
        val server = pdfServer(pages = 60)
        val device = device(tmp.root)
        val reader = controller(server, device)
        reader.open()
        advanceUntilIdle()
        reader.turn(1)
        // Not advanced: the prefetch and prune around page 2 are still to run.
        reader.goTo(50)
        advanceUntilIdle()
        val showing = reader.state.value as PdfState.Showing
        assertEquals(50, showing.page)
        assertTrue("the page on screen was pruned", showing.file.isFile)
        assertNotNull(device.pages.get(server.bookFileId, 51))
        assertNull(device.pages.get(server.bookFileId, 2))
    }

    /** Closed with a prefetch in flight: nothing of a borrowed book is left behind in the cache. */
    @Test
    fun `closing stops the fetches still running and leaves no pages behind`() = runTest {
        val server = pdfServer(pages = 20)
        val device = device(tmp.root)
        val reader = controller(server, device)
        reader.open()
        advanceUntilIdle()
        // The prefetch of page 3 waits in the air until the reader has closed.
        val gate = kotlinx.coroutines.CompletableDeferred<Unit>()
        server.pageGate = gate
        reader.turn(1)
        runCurrent()
        assertTrue("page 3 was not being fetched", server.seen.any { it.url.encodedPath.endsWith("/page/3") })
        val closing = launch { reader.send() }
        runCurrent()
        gate.complete(Unit)
        advanceUntilIdle()
        assertTrue(closing.isCompleted)
        for (n in 1..20) assertNull("page $n left in the cache", device.pages.get(server.bookFileId, n))
        // And nothing starts once closed.
        reader.goTo(5)
        advanceUntilIdle()
        assertNull(device.pages.get(server.bookFileId, 5))
    }

    @Test
    fun `past the first or last page nothing happens`() = runTest {
        val server = pdfServer(pages = 2)
        val device = device(tmp.root)
        val reader = controller(server, device)
        reader.open()
        advanceUntilIdle()
        reader.turn(-1)
        advanceUntilIdle()
        assertEquals(1, (reader.state.value as PdfState.Showing).page)
        reader.goTo(2)
        advanceUntilIdle()
        reader.turn(1)
        reader.goTo(3)
        reader.goTo(0)
        advanceUntilIdle()
        assertEquals(2, (reader.state.value as PdfState.Showing).page)
    }

    /** The page on screen stays; the one asked for is said to be missing. */
    @Test
    fun `a page that cannot be fetched leaves the reader where it was`() = runTest {
        val server = pdfServer()
        val device = device(tmp.root)
        val reader = controller(server, device)
        reader.open()
        advanceUntilIdle()
        server.offline = true
        reader.goTo(4)
        advanceUntilIdle()
        assertEquals(1, (reader.state.value as PdfState.Showing).page)
        assertNull(reader.turning.value)
        assertTrue(reader.notice.value!!.startsWith("Page 4 could not be loaded"))
    }

    /** Page by page needs to know where the book ends. */
    @Test
    fun `a PDF with no page count and no kept copy says so`() = runTest {
        val server = pdfServer(unitCount = null)
        val reader = controller(server, device(tmp.root))
        reader.open()
        advanceUntilIdle()
        assertTrue((reader.state.value as PdfState.Failed).message.contains("no page count"))
    }

    @Test
    fun `a kept PDF is read from its file with no network, where this device left it`() = runTest {
        val document = ("%PDF-1.7\n" + "z".repeat(300)).toByteArray()
        val server = pdfServer(canDownload = true)
        server.file = document
        server.info = server.info.replace("\"sizeBytes\":100", "\"sizeBytes\":${document.size}")
        val device = device(tmp.root)
        device.kept.download(server.client(this), server.bookFileId)
        device.outbox.put(PushedPosition(server.bookFileId, "pdf:4:0", 0.8, "2026-09-24T09:00:00Z"))

        server.offline = true
        server.seen.clear()
        val reader = controller(server, device)
        reader.open()
        advanceUntilIdle()

        val showing = reader.state.value as PdfState.Showing
        assertEquals(4, showing.page)
        assertEquals("page 4 of the whole book", 3, showing.index)
        assertArrayEquals(document, showing.file.readBytes())
        assertTrue(pageRequests(server).isEmpty())
        assertEquals("Two", reader.toc.value.first().title)

        // The renderer counts the file's own pages, and that wins.
        reader.documentPages(4)
        assertEquals(4, (reader.state.value as PdfState.Showing).pageCount)
        reader.turn(1)
        advanceUntilIdle()
        assertEquals(4, (reader.state.value as PdfState.Showing).page)
    }

    /** Written before it is sent: a page read on a plane is on disk until it lands. */
    @Test
    fun `leaving records the page at once, not a moment later`() = runTest {
        val server = pdfServer()
        val device = device(tmp.root)
        val reader = controller(server, device)
        reader.open()
        advanceUntilIdle()
        server.offline = true
        reader.turn(1)
        // The turn, and not the second the reader waits on a page before
        // recording it.
        runCurrent()
        reader.send()
        assertEquals("pdf:2:0", device.outbox.unsent().single().position)
    }
}
