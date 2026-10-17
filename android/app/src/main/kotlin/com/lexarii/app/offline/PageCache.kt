// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.offline

import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import java.io.File

/**
 * PDF pages being read from the server, one small PDF each, as files.
 *
 * Files because the platform's PDF renderer reads a seekable file descriptor
 * and nothing else. They live in the app's own cache directory - never shared
 * storage - and only around the page being read: this is a window onto a
 * book, not a copy of it. A book kept on the device is a different thing,
 * in the OfflineStore.
 *
 * Written whole and renamed into place, so the renderer never opens half a
 * page.
 */
class PageCache(
    private val dir: File,
    // Injectable so a test can run the disk work on its own scheduler.
    private val io: CoroutineDispatcher = Dispatchers.IO,
) {
    private val lock = Mutex()
    private var closed = false

    /** The page's file if it is here. */
    suspend fun get(bookFileId: String, page: Int): File? = withContext(io) {
        file(bookFileId, page).takeIf { it.isFile }
    }

    /** Keeps a page and returns its file, or null once signed out. */
    suspend fun put(bookFileId: String, page: Int, bytes: ByteArray): File? = lock.withLock {
        if (closed) return@withLock null
        withContext(io) { file(bookFileId, page).also { it.writeAtomically(bytes) } }
    }

    /**
     * Drops every page of this BookFile that `keep` does not want.
     *
     * `keep` is asked page by page, as it is now, so a page the reader has
     * jumped to since the prune began - fetched, about to be drawn - is kept
     * rather than deleted from under it. Stopped as soon as the prune is
     * cancelled, which a jump elsewhere does.
     */
    suspend fun keepOnly(bookFileId: String, keep: (Int) -> Boolean) = lock.withLock {
        withContext(io) {
            for (f in bookFileDir(bookFileId).listFiles().orEmpty()) {
                coroutineContext.ensureActive()
                val page = f.name.removeSuffix(".pdf").toIntOrNull()
                if (page == null || !keep(page)) f.delete()
            }
        }
    }

    /** Every page of one BookFile, for when its reader closes. */
    suspend fun drop(bookFileId: String) = lock.withLock {
        withContext(io) { bookFileDir(bookFileId).deleteRecursively() }
    }

    suspend fun clear() = lock.withLock {
        closed = true
        withContext(io) { dir.deleteRecursively() }
    }

    suspend fun reopen() = lock.withLock { closed = false }

    // A BookFile's directory is named by a hash of its id, never the id itself.
    private fun bookFileDir(bookFileId: String) = File(dir, hashed(bookFileId))
    private fun file(bookFileId: String, page: Int): File {
        require(page >= 1) { "pages are one-based: $page" }
        return File(bookFileDir(bookFileId), "$page.pdf")
    }
}
