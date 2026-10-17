// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app

import com.lexarii.app.api.LibraryItem
import com.lexarii.app.api.NotSignedIn
import com.lexarii.app.api.LexariiClient
import com.lexarii.app.offline.ShelfCache
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock

/**
 * The whole library, a page at a time.
 *
 * The first page is small, so the shelf appears quickly; the rest come in
 * bigger pages behind it, sooner if the list is scrolled towards its end.
 * One round of cursors from the first page to the last, never mixed with
 * another: a cursor names a place in one listing, and following it from a
 * different one would skip or repeat books.
 *
 * The saved shelf is replaced only by a COMPLETE listing. A round cut short -
 * the network gone at page three - leaves the last complete one in place, so
 * an offline launch never shows a library that has shrunk to its first page.
 */
class ShelfPager(
    private val client: LexariiClient,
    private val cache: ShelfCache,
    private val firstLimit: Int = 50,
    private val pageLimit: Int = 200,
) {
    /** What has arrived so far, and whether there is more to come. */
    data class Listing(
        val items: List<LibraryItem> = emptyList(),
        val complete: Boolean = false,
        val loading: Boolean = false,
        /** Why the last page did not arrive; cleared by the next that does. */
        val failed: String? = null,
    )

    private val _listing = MutableStateFlow(Listing())
    val listing: StateFlow<Listing> = _listing.asStateFlow()

    // One page in flight at a time: the list scrolling and the fill behind it
    // both ask for "the next page", and two of them would ask for the same one.
    private val lock = Mutex()
    private val items = LinkedHashMap<String, LibraryItem>()
    private var cursor: String? = null
    private var started = false

    /** When the sync pull finished, saved with the shelf for the offline screen to show. */
    @Volatile
    var syncedAt: String? = null

    /** The first page. Throws if the server could not be asked: the caller decides what offline means. */
    suspend fun first(): Listing = lock.withLock {
        if (!started) {
            page(firstLimit)
            started = true
        }
        _listing.value
    }

    /**
     * The next page, if there is one. False once the listing is complete, or
     * when the page could not be fetched - which is kept as `failed` for the
     * screen, not thrown: the books already listed are still worth showing.
     * A lapsed sign-in is thrown, because that one the app must act on.
     */
    suspend fun more(): Boolean = lock.withLock {
        if (!started || _listing.value.complete) return@withLock false
        _listing.value = _listing.value.copy(loading = true)
        try {
            page(pageLimit)
            !_listing.value.complete
        } catch (e: CancellationException) {
            _listing.value = _listing.value.copy(loading = false)
            throw e
        } catch (e: NotSignedIn) {
            _listing.value = _listing.value.copy(loading = false)
            throw e
        } catch (e: Exception) {
            _listing.value = _listing.value.copy(loading = false, failed = e.message ?: "Could not load the rest of the library")
            false
        }
    }

    /** Every page after the first, until there are none or one fails. */
    suspend fun rest() {
        while (more()) Unit
    }

    /** Saves the listing if it is complete. Called again once the sync time is known. */
    suspend fun saveIfComplete() {
        val now = _listing.value
        if (now.complete) cache.save(ShelfCache.Shelf(now.items, syncedAt))
    }

    private suspend fun page(limit: Int) {
        val page = client.library(limit = limit, cursor = cursor)
        // By BookFile: a book added between two pages can in principle be
        // listed twice, and the list is keyed by it - a repeat would crash it.
        for (item in page.items) items.putIfAbsent(item.bookFileId, item)
        val next = page.nextCursor?.takeIf { it.isNotEmpty() }
        // A server handing back the cursor it was given would be asked for
        // the same page forever. The listing stops there, as complete as the
        // server made it.
        val stuck = next != null && next == cursor
        cursor = next
        val complete = next == null || stuck
        _listing.value = Listing(items.values.toList(), complete = complete, loading = false)
        if (complete) cache.save(ShelfCache.Shelf(items.values.toList(), syncedAt))
    }
}
