// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.offline

import com.lexarii.app.api.PushedPosition
import com.lexarii.app.api.LexariiClient
import com.lexarii.app.api.SyncPosition
import com.lexarii.app.api.SyncPush
import kotlinx.coroutines.CancellationException
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
 * Where this device last was in each BookFile, and whether the server knows.
 *
 * Written to disk before anything is sent, so a position survives the app
 * being killed, a dead battery, and a week without a network. One entry per
 * BookFile: an older place that a newer one replaces is worth nothing, and
 * the server merges by the reading time each one carries, not by arrival.
 *
 * Sent entries are kept rather than dropped. They are this device's own
 * memory of where it was, which is what opening a kept copy on a plane has to
 * go on.
 */
class Outbox(
    private val file: File,
    // Injectable so a test can run the disk work on its own scheduler.
    private val io: CoroutineDispatcher = Dispatchers.IO,
) {
    @Serializable
    data class Entry(val position: PushedPosition, val sent: Boolean)

    private val json = Json { ignoreUnknownKeys = true; explicitNulls = false }
    private val lock = Mutex()
    private var entries: MutableMap<String, Entry>? = null

    // Set by clear() until reopen(): a report still in flight for the account
    // that signed out must not land in the next one's outbox.
    private var closed = false

    /** Records a position as unsent. It is on disk when this returns. */
    suspend fun put(position: PushedPosition) = lock.withLock {
        val all = load()
        all[position.bookFileId] = Entry(position, sent = false)
        save(all)
    }

    /** This device's last position in a BookFile, sent or not. */
    suspend fun last(bookFileId: String): PushedPosition? = lock.withLock { load()[bookFileId]?.position }

    /**
     * Positions a sync pull brought from other devices, remembered as where
     * this person was - so a kept copy opened on a plane opens where the
     * laptop left it, not where this phone last was.
     *
     * Only over a SENT entry, and only a newer one. An unsent position here
     * still has something to tell the server even when it is older: how far
     * the reader got only ever advances, and this may be the furthest.
     */
    suspend fun learn(positions: List<SyncPosition>) {
        if (positions.isEmpty()) return
        lock.withLock {
            val all = load()
            var changed = false
            for (p in positions) {
                val local = all[p.bookFileId]
                if (local != null && !local.sent) continue
                val theirs = MarkStore.instantOf(p.readAt) ?: continue
                val ours = local?.let { MarkStore.instantOf(it.position.readAt) }
                if (ours != null && !theirs.isAfter(ours)) continue
                all[p.bookFileId] = Entry(PushedPosition(p.bookFileId, p.position, p.percent, p.readAt), sent = true)
                changed = true
            }
            if (changed) save(all)
        }
    }

    suspend fun unsent(): List<PushedPosition> = lock.withLock { load().values.filter { !it.sent }.map { it.position } }

    /**
     * Sends everything unsent in one push. True if nothing is left unsent.
     *
     * An entry is marked sent only if it is still the one that went: a newer
     * position recorded while the request was in the air stays unsent and
     * goes next time.
     */
    suspend fun flush(client: LexariiClient): Boolean {
        val pending = unsent()
        if (pending.isEmpty()) return true
        try {
            client.push(SyncPush(positions = pending))
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            return false
        }
        return lock.withLock {
            val all = load()
            for (p in pending) {
                if (all[p.bookFileId]?.position == p) all[p.bookFileId] = Entry(p, sent = true)
            }
            save(all)
            all.values.none { !it.sent }
        }
    }

    /** Empties the outbox and closes it to writes until [reopen]. */
    suspend fun clear() = lock.withLock {
        closed = true
        entries = null
        withContext(io) { file.delete() }
    }

    suspend fun reopen() = lock.withLock {
        closed = false
        entries = null
    }

    private suspend fun load(): MutableMap<String, Entry> {
        entries?.let { return it }
        val read = withContext(io) {
            if (!file.isFile) return@withContext null
            runCatching { json.decodeFromString<Map<String, Entry>>(file.readText()) }.getOrNull()
        }
        return (read?.toMutableMap() ?: mutableMapOf()).also { entries = it }
    }

    private suspend fun save(all: Map<String, Entry>) {
        if (closed) return
        withContext(io) { file.writeAtomically(json.encodeToString(all).toByteArray()) }
    }
}
