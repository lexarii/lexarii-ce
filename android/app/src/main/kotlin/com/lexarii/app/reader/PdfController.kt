// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import com.lexarii.app.api.PushedPosition
import com.lexarii.app.api.LexariiClient
import com.lexarii.app.api.TOCEntry
import com.lexarii.app.offline.OfflineStore
import com.lexarii.app.offline.Outbox
import com.lexarii.app.offline.PageCache
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import java.io.File
import java.time.Instant

/** What the PDF reader is showing. */
sealed interface PdfState {
    data object Opening : PdfState
    data class Failed(val message: String) : PdfState

    /**
     * One page, as a file the platform renderer can open and the page's index
     * inside it: a kept PDF is the whole book, where page 12 is index 11; a
     * page from the server is a one-page document, always index 0.
     *
     * `pageCount` is 0 when nothing has said how many pages there are yet - a
     * kept file whose record lacked a count, until the renderer has opened it.
     */
    data class Showing(
        val page: Int,
        val pageCount: Int,
        val file: File,
        val index: Int,
        val generation: Int,
    ) : PdfState
}

/**
 * One open PDF: which page, where it comes from, and what gets sent back.
 *
 * A kept copy is read straight from its file. Otherwise pages come from the
 * server one at a time, as the web's reader fetches them for a book lent to be
 * read: the page asked for, then its neighbours marked as prefetches so the
 * server does not record them as read. Those pages sit in the cache only
 * around where the reader is.
 *
 * The position goes to the server through the outbox and the sync push, as
 * an EPUB's does - `pdf:<page>:0`, a locator and never a percentage - on disk
 * before it is sent.
 *
 * Highlights are not made in PDFs here: a rendered page has no text to select.
 */
class PdfController(
    val bookFileId: String,
    private val client: LexariiClient,
    private val kept: OfflineStore,
    private val pages: PageCache,
    private val outbox: Outbox,
    private val scope: CoroutineScope,
    private val clock: () -> Instant = Instant::now,
) {
    private val _state = MutableStateFlow<PdfState>(PdfState.Opening)
    val state: StateFlow<PdfState> = _state.asStateFlow()

    /** The book's contents, where it has one. Arrives when it arrives; nobody waits on it. */
    private val _toc = MutableStateFlow<List<TOCEntry>>(emptyList())
    val toc: StateFlow<List<TOCEntry>> = _toc.asStateFlow()

    /** The page being fetched, while the one before it stays on screen. */
    private val _turning = MutableStateFlow<Int?>(null)
    val turning: StateFlow<Int?> = _turning.asStateFlow()

    private val _notice = MutableStateFlow<String?>(null)
    val notice: StateFlow<String?> = _notice.asStateFlow()

    fun noticeSeen() {
        _notice.value = null
    }

    private var file: File? = null
    private var pageCount = 0

    // Read from the disk thread by the prune, which must not delete the page
    // on screen.
    @Volatile
    private var page = 0
    private var generation = 0
    private var load: Job? = null
    private var ahead: Job? = null
    private var settle: Job? = null
    private var contentsJob: Job? = null
    private var lastRequest: (() -> Unit)? = null

    // Set on the way out. Nothing starts after it: a page of a borrowed book
    // fetched once the reader has closed would sit in the cache after the
    // cache was emptied for it.
    private var closed = false

    fun open() {
        if (closed) return
        lastRequest = ::open
        _state.value = PdfState.Opening
        load?.cancel()
        load = scope.launch {
            try {
                val copy = kept.pdf(bookFileId)
                file = copy
                val info = (if (copy != null) kept.info(bookFileId) else null) ?: if (copy == null) client.info(bookFileId) else null
                if (info != null && info.format != "pdf") throw IllegalStateException("This is not a PDF")
                pageCount = info?.unitCount ?: 0
                // Pages one at a time need to know where the book ends; a kept
                // file can tell the renderer itself.
                if (copy == null && pageCount <= 0) throw IllegalStateException("This PDF has no page count recorded yet")
                contentsJob?.cancel()
                contentsJob = scope.launch { contents() }

                val start = PdfLocator.parse(resumeFrom(outbox.last(bookFileId), client, bookFileId, copy != null, SERVER_PATIENCE_MS))?.page
                showNow(clamp(start ?: 1))
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                _state.value = PdfState.Failed(e.message ?: "Could not open this book")
            }
        }
    }

    fun retry() {
        lastRequest?.invoke()
    }

    /** One page forward or back. Past either end of the book, nothing happens. */
    fun turn(direction: Int) {
        val showing = _state.value as? PdfState.Showing ?: return
        goTo((_turning.value ?: showing.page) + direction.coerceIn(-1, 1))
    }

    /** A page from the contents, or anywhere. Pages outside the book are ignored. */
    fun goTo(target: Int) {
        if (closed || target < 1 || (pageCount > 0 && target > pageCount)) return
        val showing = _state.value as? PdfState.Showing
        if (showing != null && showing.page == target && _turning.value == null) return
        load?.cancel()
        // The prefetch and prune around the page being left are for a place
        // the reader no longer is.
        ahead?.cancel()
        _turning.value = target
        load = scope.launch {
            try {
                showNow(target)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                _turning.value = null
                if (_state.value is PdfState.Showing) {
                    // The page on screen stays; the one asked for is said to be missing.
                    _notice.value = "Page $target could not be loaded. ${e.message.orEmpty()}".trim()
                } else {
                    lastRequest = { goTo(target) }
                    _state.value = PdfState.Failed(e.message ?: "Could not load page $target")
                }
            }
        }
    }

    /**
     * How many pages the renderer found in a kept file. The file is the
     * authority on itself; a record that said otherwise is out of date.
     */
    fun documentPages(count: Int) {
        if (count <= 0 || file == null || count == pageCount) return
        pageCount = count
        val showing = _state.value as? PdfState.Showing ?: return
        if (showing.page > count) goTo(count) else _state.value = showing.copy(pageCount = count)
    }

    /**
     * Sends where the reader is now, not a moment from now, and lets go of
     * the pages. Called on the way out.
     *
     * Everything still fetching is stopped and waited for first, so no page
     * lands in the cache after it has been emptied.
     */
    suspend fun send() {
        closed = true
        for (job in listOfNotNull(load, ahead, settle, contentsJob)) job.cancelAndJoin()
        record()
        outbox.flush(client)
        if (file == null) pages.drop(bookFileId)
    }

    private suspend fun showNow(target: Int) {
        val kept = file
        val (source, index) = if (kept != null) {
            kept to target - 1
        } else {
            (pages.get(bookFileId, target) ?: fetch(target, prefetch = false)) to 0
        }
        page = target
        _turning.value = null
        _state.value = PdfState.Showing(target, pageCount, source, index, ++generation)
        settled()
        if (kept == null) around(target)
    }

    private suspend fun fetch(target: Int, prefetch: Boolean): File {
        val bytes = client.pdfPage(bookFileId, target, prefetch)
        return pages.put(bookFileId, target, bytes) ?: throw IllegalStateException("signed out")
    }

    /**
     * The neighbours, fetched ahead so a turn is immediate, and every page
     * further away let go. Never awaited, and a failure is nobody's concern:
     * nobody asked for these pages yet.
     */
    private fun around(centre: Int) {
        ahead?.cancel()
        ahead = scope.launch {
            for (n in listOf(centre + 1, centre - 1)) {
                if (n < 1 || n > pageCount || pages.get(bookFileId, n) != null) continue
                try {
                    fetch(n, prefetch = true)
                } catch (e: CancellationException) {
                    throw e
                } catch (e: Exception) {
                    // A missing neighbour is fetched for real when it is turned to.
                }
            }
            val window = (centre - KEEP_AROUND)..(centre + KEEP_AROUND)
            // Asked page by page, as things are then: the page on screen and
            // one being fetched are kept even if the reader has jumped away
            // from this window since.
            pages.keepOnly(bookFileId) { n -> n in window || n == page || n == _turning.value }
        }
    }

    /**
     * The page is recorded once the reader has stayed on it a moment, like the
     * web's: flicking through ten pages is one position, not ten.
     */
    private fun settled() {
        settle?.cancel()
        settle = scope.launch {
            delay(SETTLE_MS)
            record()
            outbox.flush(client)
        }
    }

    private suspend fun record() {
        if (page < 1) return
        val position = PushedPosition(
            bookFileId = bookFileId,
            position = PdfLocator(page).format(),
            percent = if (pageCount > 0) (page.toDouble() / pageCount).coerceIn(0.0, 1.0) else null,
            readAt = clock().toString(),
        )
        // Recorded even when it names the page already recorded: the time is
        // news too. Another device may have moved on since, and coming back
        // to this page is the newer place.
        outbox.put(position)
    }

    private suspend fun contents() {
        val manifest = try {
            kept.manifest(bookFileId) ?: client.manifest(bookFileId)
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            null // a PDF is perfectly readable without a contents list
        }
        _toc.value = manifest?.toc.orEmpty()
    }

    private fun clamp(target: Int): Int = if (pageCount > 0) target.coerceIn(1, pageCount) else maxOf(1, target)

    private companion object {
        const val SERVER_PATIENCE_MS = 3_000L
        const val SETTLE_MS = 1_000L

        // Two either side: the neighbours prefetched, and one more so that
        // turning back and forth over a page boundary does not fetch it again.
        const val KEEP_AROUND = 2
    }
}
