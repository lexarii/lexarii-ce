// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.offline

import com.lexarii.app.testing.FakeServer
import com.lexarii.app.testing.device
import kotlinx.coroutines.test.TestScope
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

/**
 * A PDF kept on the device: the file itself, fetched in one stream and
 * carried on with a range request when the stream breaks.
 */
class PdfKeepTest {
    @get:Rule
    val tmp = TemporaryFolder()

    private val document = ("%PDF-1.7\n" + "x".repeat(5000) + "\n%%EOF\n").toByteArray()

    private fun pdfServer(bytes: ByteArray = document, canDownload: Boolean = true) = FakeServer().apply {
        manifest = """{"bookFileId":"$bookFileId","format":"pdf","spine":[],"toc":[{"title":"One","spineIndex":2}]}"""
        info = """{"bookFileId":"$bookFileId","title":"A PDF","workId":"w1","format":"pdf","sizeBytes":${bytes.size},""" +
            """"status":"ready","unitCount":9,"reflowable":false,"canDownload":$canDownload}"""
        file = bytes
    }

    private suspend fun TestScope.keep(server: FakeServer, device: Device) = device.kept.download(server.client(this), server.bookFileId)

    @Test
    fun `a PDF is kept as its file, with its contents and page count`() = runTest {
        val server = pdfServer()
        val device = device(tmp.root)
        var last = 0L to 0L
        device.kept.download(server.client(this), server.bookFileId) { done, total, bytes ->
            assertTrue("a PDF counts bytes", bytes)
            last = done to total
        }
        assertTrue(device.kept.has(server.bookFileId))
        assertArrayEquals(document, device.kept.pdf(server.bookFileId)!!.readBytes())
        assertEquals(9, device.kept.info(server.bookFileId)?.unitCount)
        assertEquals(2, device.kept.manifest(server.bookFileId)?.toc?.single()?.spineIndex)
        assertEquals(document.size.toLong() to document.size.toLong(), last)
        // The partial's bookkeeping does not come along.
        assertTrue(tmp.root.walk().none { it.name == "validator" || it.name == "started.json" })
    }

    @Test
    fun `a broken stream is carried on from where it stopped`() = runTest {
        val server = pdfServer()
        server.cutFileAfter = 1000
        val device = device(tmp.root)
        try {
            keep(server, device)
            fail("a short file was kept")
        } catch (e: java.io.IOException) {
            // expected
        }
        assertFalse(device.kept.has(server.bookFileId))

        server.seen.clear()
        keep(server, device)
        val resumed = server.seen.single { it.url.encodedPath.endsWith("/file") }
        assertEquals("bytes=1000-", resumed.headers["Range"])
        assertEquals("only if the file has not changed", server.lastModified, resumed.headers["If-Range"])
        assertArrayEquals(document, device.kept.pdf(server.bookFileId)!!.readBytes())
    }

    /** The server sends it whole when the file changed; the two versions are never spliced. */
    @Test
    fun `a file that changed between tries is fetched again whole`() = runTest {
        val server = pdfServer()
        server.cutFileAfter = 1000
        val device = device(tmp.root)
        runCatching { keep(server, device) }

        val changed = ("%PDF-1.7\n" + "y".repeat(7000) + "\n%%EOF\n").toByteArray()
        server.file = changed
        server.lastModified = "Thu, 25 Sep 2026 08:00:00 GMT"
        server.info = server.info.replace("\"sizeBytes\":${document.size}", "\"sizeBytes\":${changed.size}")
        keep(server, device)
        assertArrayEquals(changed, device.kept.pdf(server.bookFileId)!!.readBytes())
    }

    /**
     * Replaced by a copy that kept its modification time - rsync -a, cp -p -
     * the validator still matches. The length does not, and that is enough
     * not to splice the new file's tail onto the old one's start.
     */
    @Test
    fun `a file replaced under the same time is not spliced, it is fetched again`() = runTest {
        val server = pdfServer()
        server.cutFileAfter = 1000
        val device = device(tmp.root)
        runCatching { keep(server, device) }

        val replaced = ("%PDF-1.7\n" + "q".repeat(8000) + "\n%%EOF\n").toByteArray()
        server.file = replaced
        server.info = server.info.replace("\"sizeBytes\":${document.size}", "\"sizeBytes\":${replaced.size}")
        try {
            keep(server, device)
            fail("resumed onto a different file")
        } catch (e: java.io.IOException) {
            assertTrue(e.message!!, e.message!!.contains("changed on the server"))
        }
        keep(server, device)
        assertArrayEquals(replaced, device.kept.pdf(server.bookFileId)!!.readBytes())
    }

    /** A part that starts somewhere else would be appended in the wrong place. */
    @Test
    fun `a part that does not start where it was asked for is refused`() = runTest {
        val server = pdfServer()
        server.cutFileAfter = 1000
        val device = device(tmp.root)
        runCatching { keep(server, device) }
        server.rangeSkew = 10
        try {
            keep(server, device)
            fail("appended a part from the wrong place")
        } catch (e: java.io.IOException) {
            assertTrue(e.message!!, e.message!!.contains("from byte 1010"))
        }
        server.rangeSkew = 0
        keep(server, device)
        assertArrayEquals(document, device.kept.pdf(server.bookFileId)!!.readBytes())
    }

    @Test
    fun `something that is not a PDF is not kept, and not resumed either`() = runTest {
        val html = "<html>a login page, perhaps</html>".toByteArray()
        val server = pdfServer(html)
        val device = device(tmp.root)
        try {
            keep(server, device)
            fail("kept a file that is not a PDF")
        } catch (e: java.io.IOException) {
            // expected
        }
        assertFalse(device.kept.has(server.bookFileId))
        assertTrue(tmp.root.walk().none { it.name == "file.pdf" })
    }

    /** A book lent to be read is never taken as a file. */
    @Test
    fun `a PDF shared to be read is not kept`() = runTest {
        val server = pdfServer(canDownload = false)
        val device = device(tmp.root)
        try {
            keep(server, device)
            fail("kept a book shared to be read")
        } catch (e: IllegalArgumentException) {
            // expected
        }
        assertTrue(server.seen.none { it.url.encodedPath.endsWith("/file") })
        assertNull(device.kept.pdf(server.bookFileId))
    }
}
