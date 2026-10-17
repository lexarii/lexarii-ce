// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app

import com.lexarii.app.offline.ShelfCache
import com.lexarii.app.testing.FakeServer
import com.lexarii.app.testing.device
import com.lexarii.app.testing.libraryItemJson
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

/** The whole library, by cursor, and a saved shelf that never shrinks to a page. */
class ShelfPagerTest {
    @get:Rule
    val tmp = TemporaryFolder()

    private fun ids(n: Int) = (1..n).map { "b-%04d".format(it) }

    private fun server(n: Int) = FakeServer().apply { library = ids(n).map { libraryItemJson(it) } }

    private fun requests(server: FakeServer) =
        server.seen.filter { it.url.encodedPath == "/api/v1/library" }.map { it.url.parameters["limit"] to it.url.parameters["cursor"] }

    @Test
    fun `the first page is small and the rest follow it by cursor`() = runTest {
        val server = server(430)
        val pager = ShelfPager(server.client(this), device(tmp.root).shelf)

        val first = pager.first()
        assertEquals(50, first.items.size)
        assertFalse(first.complete)

        pager.rest()
        val all = pager.listing.value
        assertEquals(ids(430), all.items.map { it.bookFileId })
        assertTrue(all.complete)
        assertEquals(
            listOf("50" to null, "200" to "after-50", "200" to "after-250"),
            requests(server),
        )
        // Asking again once complete asks nothing.
        assertFalse(pager.more())
        assertEquals(3, requests(server).size)
    }

    /** An offline launch shows the last COMPLETE shelf, never the first page of an unfinished one. */
    @Test
    fun `the saved shelf is replaced only by a complete listing`() = runTest {
        val device = device(tmp.root)
        val before = server(300)
        val first = ShelfPager(before.client(this), device.shelf)
        first.first()
        first.rest()
        assertEquals(300, device.shelf.load()?.items?.size)

        val after = server(320)
        after.failLibraryAt = "after-250"
        val second = ShelfPager(after.client(this), device.shelf)
        second.first()
        assertEquals("the first page saved over the full shelf", 300, device.shelf.load()?.items?.size)
        second.rest()
        assertEquals(250, second.listing.value.items.size)
        assertEquals("boom", second.listing.value.failed)
        assertEquals("a broken round saved over the full shelf", 300, device.shelf.load()?.items?.size)

        // Carried on from where it broke, not from the start.
        after.failLibraryAt = null
        after.seen.clear()
        second.rest()
        assertEquals(listOf("200" to "after-250"), requests(after))
        assertEquals(320, device.shelf.load()?.items?.size)
        assertNull(second.listing.value.failed)
    }

    @Test
    fun `a small library is complete, and saved, from its first page`() = runTest {
        val device = device(tmp.root)
        val pager = ShelfPager(server(12).client(this), device.shelf)
        pager.syncedAt = "2026-09-25T10:00:00Z"
        assertTrue(pager.first().complete)
        assertEquals(ShelfCache.Shelf::class, device.shelf.load()!!::class)
        assertEquals(12, device.shelf.load()?.items?.size)
        assertEquals("2026-09-25T10:00:00Z", device.shelf.load()?.syncedAt)
    }

    /** The list is keyed by BookFile, and a repeat would crash it. */
    @Test
    fun `a BookFile listed twice across pages is shown once`() = runTest {
        val server = server(60)
        server.library = server.library + libraryItemJson("b-0001")
        val pager = ShelfPager(server.client(this), device(tmp.root).shelf)
        pager.first()
        pager.rest()
        assertEquals(60, pager.listing.value.items.size)
        assertEquals(60, pager.listing.value.items.map { it.bookFileId }.toSet().size)
    }
}
