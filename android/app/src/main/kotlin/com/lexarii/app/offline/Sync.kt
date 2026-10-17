// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.offline

import com.lexarii.app.api.LexariiClient
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.io.File

/**
 * The pull half of the sync protocol, and the cursor it runs from.
 *
 * The cursor is the server's `until` from the last pull that finished, kept
 * on disk so the next run asks only for what changed since. It is the
 * server's clock, never this device's: two clocks a minute apart would
 * silently lose or repeat a minute of somebody's edits.
 */
class Sync(
    private val file: File,
    private val marks: MarkStore,
    private val outbox: Outbox,
    // Injectable so a test can run the disk work on its own scheduler.
    private val io: CoroutineDispatcher = Dispatchers.IO,
    // The server's own ceiling, so there are as few pages as possible; a test
    // asks for small pages to see the paging at all.
    private val pageSize: Int = 1000,
) {
    @Serializable
    private data class Cursor(val since: String)

    private val json = Json { ignoreUnknownKeys = true }
    // One pull at a time: the library and a reader opening can both start one,
    // and two writing the cursor in either order could store the older.
    private val lock = Mutex()

    // The cursor file and whether it may be written. Its own lock, not the
    // pull's: clearing must not wait on a pull stuck on a dead network, and a
    // pull that comes back after the clear must find it closed.
    private val cursorLock = Mutex()
    private var closed = false

    /** Where the next pull starts, or null for everything. */
    suspend fun since(): String? = withContext(io) {
        if (!file.isFile) return@withContext null
        runCatching { json.decodeFromString<Cursor>(file.readText()).since }.getOrNull()
    }

    /**
     * Everything that changed since the last pull, taken in. Returns the
     * `since` now stored. Throws if the server could not be asked, having kept
     * whatever pages did arrive - every apply is by id, so they are simply
     * applied again next time.
     *
     * One round: the first call from the stored `since`, then each page after
     * it by the `cursor` the page before handed back, verbatim. Only when a
     * page says there is no `more` is its `until` stored as the next round's
     * `since`: storing an earlier one would skip everything that did not fit.
     *
     * The paging cursor itself is never stored. It belongs to one round: a
     * round cut short starts again from the stored `since`, which re-reads the
     * overlap the server keeps at the start of every round, and what comes
     * again is applied again harmlessly. Only the last page's `until` is kept,
     * so it does not matter which moment the server puts on the pages before.
     */
    suspend fun pull(client: LexariiClient): String? = lock.withLock {
        val stored = since()
        var cursor: String? = null
        repeat(MAX_PAGES) {
            val page = client.pull(since = stored, cursor = cursor, limit = pageSize)
            marks.applyPulled(page.annotations, page.deleted)
            outbox.learn(page.positions)
            if (!page.more) {
                cursorLock.withLock {
                    if (closed) return@withLock
                    withContext(io) { file.writeAtomically(json.encodeToString(Cursor(page.until)).toByteArray()) }
                }
                return@withLock page.until
            }
            // More, with no way to ask for it - or the same place again, which
            // would be asked for forever. Either is a server fault; the round
            // stops unfinished rather than guess, and the stored `since` stays.
            val next = page.cursor ?: return@withLock stored
            if (next == cursor) return@withLock stored
            cursor = next
        }
        stored
    }

    /** Forgets the cursor and closes it to writes until [reopen]. */
    suspend fun clear() = cursorLock.withLock {
        closed = true
        withContext(io) { file.delete() }
    }

    suspend fun reopen() = cursorLock.withLock { closed = false }

    private companion object {
        // A safety net, not a limit anyone should meet: a thousand pages of a
        // thousand changes is far past any one person's reading. It is there
        // so a server that says `more` forever, with a new cursor each time,
        // cannot keep a phone asking.
        const val MAX_PAGES = 1000
    }
}
