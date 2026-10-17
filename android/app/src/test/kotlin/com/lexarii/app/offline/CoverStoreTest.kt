// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.offline

import com.lexarii.app.testing.FakeServer
import kotlinx.coroutines.test.StandardTestDispatcher
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.io.File

/** Cover thumbnails on disk: bounded, named by hash, gone with the account. */
class CoverStoreTest {
    @get:Rule
    val tmp = TemporaryFolder()

    @Test
    fun `covers are kept under a hash of their id and read back`() = runTest {
        val store = CoverStore(tmp.root, StandardTestDispatcher(testScheduler))
        store.put("../../etc/passwd", byteArrayOf(1, 2, 3))
        assertArrayEquals(byteArrayOf(1, 2, 3), store.get("../../etc/passwd"))
        assertEquals(listOf(hashed("../../etc/passwd")), tmp.root.list()!!.toList())
        assertNull(store.get("another"))
    }

    @Test
    fun `past the bound the covers looked at longest ago go first`() = runTest {
        val store = CoverStore(tmp.root, StandardTestDispatcher(testScheduler), maxBytes = 250)
        store.put("a", ByteArray(100))
        File(tmp.root, hashed("a")).setLastModified(1_000_000)
        store.put("b", ByteArray(100))
        File(tmp.root, hashed("b")).setLastModified(2_000_000)
        store.put("c", ByteArray(100))
        assertNull("the oldest was not let go", store.get("a"))
        assertNotNull(store.get("b"))
        assertNotNull(store.get("c"))
        // Larger than the whole bound: not kept at all.
        store.put("huge", ByteArray(300))
        assertNull(store.get("huge"))
    }

    /** The running total counts a replaced cover once, not twice. */
    @Test
    fun `replacing a cover does not count it twice`() = runTest {
        val store = CoverStore(tmp.root, StandardTestDispatcher(testScheduler), maxBytes = 250)
        store.put("a", ByteArray(100))
        store.put("b", ByteArray(100))
        store.put("b", ByteArray(100))
        store.put("b", ByteArray(100))
        assertNotNull("a was let go though the two fit", store.get("a"))
        assertNotNull(store.get("b"))
    }

    @Test
    fun `after signing out nothing more is kept until the next sign-in`() = runTest {
        val store = CoverStore(tmp.root, StandardTestDispatcher(testScheduler))
        store.put("a", byteArrayOf(1))
        store.clear()
        assertTrue(!tmp.root.exists() || tmp.root.list().isNullOrEmpty())
        store.put("b", byteArrayOf(2))
        assertNull(store.get("b"))
        store.reopen()
        store.put("b", byteArrayOf(2))
        assertArrayEquals(byteArrayOf(2), store.get("b"))
    }

    /** The client builds the path from the id; a listing's URL is never followed. */
    @Test
    fun `a cover is fetched from its BookFile's own path, and bounded`() = runTest {
        val server = FakeServer()
        server.covers[server.bookFileId] = byteArrayOf(9, 9)
        val client = server.client(this)
        assertArrayEquals(byteArrayOf(9, 9), client.cover(server.bookFileId).bytes)
        assertEquals("/api/v1/books/${server.bookFileId}/cover", server.seen.last().url.encodedPath)

        server.covers[server.bookFileId] = ByteArray(9 * 1024 * 1024)
        try {
            client.cover(server.bookFileId)
            fail("took a cover larger than any thumbnail needs")
        } catch (e: java.io.IOException) {
            // expected
        }
    }
}
