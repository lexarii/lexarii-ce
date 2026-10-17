// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.offline

import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import java.io.File

/**
 * Cover thumbnails, kept so the shelf has its pictures with no network and
 * does not fetch them again on every launch.
 *
 * Bounded in bytes. Past the bound, the covers looked at longest ago go
 * first; a cover is only ever a thumbnail of something the server still has,
 * so losing one costs a request, never anything of the reader's.
 *
 * One file per BookFile, named by a hash of its id.
 */
class CoverStore(
    private val dir: File,
    // Injectable so a test can run the disk work on its own scheduler.
    private val io: CoroutineDispatcher = Dispatchers.IO,
    private val maxBytes: Long = 24L * 1024 * 1024,
) {
    private val lock = Mutex()

    // Set by clear() until reopen(): a cover still arriving for the account
    // that signed out must not be kept for the next one.
    private var closed = false

    suspend fun get(bookFileId: String): ByteArray? = withContext(io) {
        val file = file(bookFileId)
        if (!file.isFile) return@withContext null
        // Touched, so the ones in use are the last to go.
        file.setLastModified(System.currentTimeMillis())
        runCatching { file.readBytes() }.getOrNull()
    }

    suspend fun put(bookFileId: String, bytes: ByteArray) = lock.withLock {
        if (closed || bytes.isEmpty() || bytes.size > maxBytes) return@withLock
        withContext(io) {
            // A running total, counted from the disk once: adding a cover
            // must not mean reading the size of every other one.
            val before = total ?: measured()
            val file = file(bookFileId)
            val replaced = if (file.isFile) file.length() else 0L
            file.writeAtomically(bytes)
            val now = before - replaced + bytes.size
            total = if (now > maxBytes) trim() else now
        }
    }

    suspend fun clear() = lock.withLock {
        closed = true
        total = null
        withContext(io) { dir.deleteRecursively() }
    }

    suspend fun reopen() = lock.withLock {
        closed = false
        total = null
    }

    // Bytes on disk, once known. Null until the first put of this run.
    private var total: Long? = null

    private fun covers() = dir.listFiles { f -> f.isFile && !f.name.startsWith(".") }.orEmpty()

    private fun measured(): Long = covers().sumOf { it.length() }

    /** Lets the least recently looked-at covers go until the rest fit. Returns what is left. */
    private fun trim(): Long {
        val files = covers()
        var left = files.sumOf { it.length() }
        for (f in files.sortedBy { it.lastModified() }) {
            if (left <= maxBytes) break
            val size = f.length()
            if (f.delete()) left -= size
        }
        return left
    }

    private fun file(bookFileId: String) = File(dir, hashed(bookFileId))
}
