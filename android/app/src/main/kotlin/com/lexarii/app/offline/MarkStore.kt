// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.offline

import com.lexarii.app.api.Annotation
import com.lexarii.app.api.DeletedMark
import com.lexarii.app.api.PushedMark
import com.lexarii.app.api.ServerRefused
import com.lexarii.app.api.LexariiClient
import com.lexarii.app.api.SyncPush
import com.lexarii.app.api.SyncPushResult
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
import java.time.Instant
import java.time.OffsetDateTime
import java.util.logging.Logger

/** Whether the server has a change yet. */
@Serializable
enum class Delivery {
    UNSENT,
    SENT,

    /**
     * The server said no - a book this person can no longer open, or a mark
     * it could not store. Kept, because it is still somebody's note, and not
     * sent again until it changes: the same push would get the same answer.
     */
    REFUSED,
}

/**
 * A highlight, note, or bookmark, as this device holds it.
 *
 * The wire's Annotation plus whether the server has this version. `shared`
 * marks are somebody else's, visible because they shared them: shown, never
 * edited, never deleted, never sent anywhere.
 */
@Serializable
data class Mark(
    val id: String,
    val bookFileId: String,
    val type: String,
    val start: String,
    val end: String,
    val selectedText: String? = null,
    val note: String? = null,
    val colour: String? = null,
    val orphaned: Boolean = false,
    val shared: Boolean = false,
    val author: String? = null,
    val createdAt: String,
    /**
     * When this version was MADE, by the clock of whoever made it - the time
     * the server's last-write-wins compares - not when any server stored it.
     */
    val updatedAt: String,
    val delivery: Delivery = Delivery.SENT,
) {
    val hasNote: Boolean get() = !note.isNullOrBlank()
}

/**
 * The marks in every BookFile, kept on the device, and the deletions the
 * server has not heard about yet.
 *
 * The same promise as the Outbox: a mark is on disk before anything is sent,
 * so a highlight made on a plane survives the app being killed before it
 * lands. The id is made here, not by the server, which is what lets the same
 * mark be pushed twice without becoming two.
 *
 * One file per BookFile, so a highlight rewrites the marks of one book rather
 * than every mark this person has ever made.
 */
class MarkStore(
    private val dir: File,
    // Injectable so a test can run the disk work on its own scheduler.
    private val io: CoroutineDispatcher = Dispatchers.IO,
) {
    /** A deletion not yet sent. It goes with the client's own time, like an edit. */
    @Serializable
    data class Tombstone(val id: String, val bookFileId: String, val deletedAt: String, val delivery: Delivery = Delivery.UNSENT)

    @Serializable
    private data class Held(
        val bookFileId: String,
        val marks: List<Mark> = emptyList(),
        val tombstones: List<Tombstone> = emptyList(),
    )

    private val json = Json { ignoreUnknownKeys = true; explicitNulls = false; encodeDefaults = true }
    private val lock = Mutex()
    private var held: MutableMap<String, Held>? = null

    /**
     * Set by clear(), until the next account opens it. Work still in flight
     * for the account that just left - a pull that had not come back yet -
     * must not write that account's notes into the next one's storage, so
     * every write after a clear is dropped.
     */
    private var closed = false

    /** Every mark in one BookFile, own and shared, in no particular order. */
    suspend fun marks(bookFileId: String): List<Mark> = lock.withLock { load()[bookFileId]?.marks.orEmpty() }

    /**
     * Records a mark made or changed on this device, as unsent. It is on disk
     * when this returns.
     *
     * Refused for a shared mark, or for one this device has deleted: the
     * first is not this person's to change, and the second would come back
     * from the dead.
     */
    suspend fun put(mark: Mark): Boolean = lock.withLock {
        if (closed) return@withLock false
        val all = load()
        val current = all[mark.bookFileId] ?: Held(mark.bookFileId)
        val existing = current.marks.firstOrNull { it.id == mark.id }
        if (mark.shared || existing?.shared == true || current.tombstones.any { it.id == mark.id }) return@withLock false
        val updated = current.copy(marks = current.marks.filter { it.id != mark.id } + mark.copy(delivery = Delivery.UNSENT))
        save(all, updated)
        true
    }

    /**
     * Deletes one of this person's own marks here, and remembers to tell the
     * server. False if there is no such mark or it is somebody else's.
     */
    suspend fun delete(bookFileId: String, id: String, at: String): Boolean = lock.withLock {
        if (closed) return@withLock false
        val all = load()
        val current = all[bookFileId] ?: return@withLock false
        val mark = current.marks.firstOrNull { it.id == id } ?: return@withLock false
        if (mark.shared) return@withLock false
        save(
            all,
            current.copy(
                marks = current.marks.filter { it.id != id },
                tombstones = current.tombstones.filter { it.id != id } + Tombstone(id, bookFileId, at),
            ),
        )
        true
    }

    /** How many changes the server has not received, for the sign-out question. */
    suspend fun unsent(): Int = lock.withLock {
        load().values.sumOf { h ->
            h.marks.count { it.delivery == Delivery.UNSENT && !it.shared } + h.tombstones.count { it.delivery == Delivery.UNSENT }
        }
    }

    /**
     * Own marks the server refused. They exist nowhere but this device, so
     * signing out loses them for good, and the question before it says so.
     */
    suspend fun refused(): Int = lock.withLock {
        load().values.sumOf { h -> h.marks.count { it.delivery == Delivery.REFUSED && !it.shared } }
    }

    /**
     * Sends every unsent change. True if nothing is left unsent.
     *
     * In batches well inside the server's limits on one push - five hundred
     * changes, two megabytes - because a push over either is refused whole,
     * and the same push would be refused again on every flush after it.
     *
     * A change is marked sent only if it is still the version that went: an
     * edit made while the request was in the air stays unsent and goes next.
     */
    suspend fun flush(client: LexariiClient): Boolean {
        val pending = lock.withLock { pendingLocked() }
        if (pending.isEmpty()) return true

        val stale = mutableMapOf<String, MutableSet<String>>()
        for (batch in batches(pending)) {
            val result = try {
                client.push(SyncPush(annotations = batch))
            } catch (e: CancellationException) {
                throw e
            } catch (e: ServerRefused) {
                // This batch, not the connection. The next may still land;
                // this one stays unsent and goes again next time. (The server
                // answers a bad mark with its own "refused", not by failing
                // the push, so this is a server fault, not a poisoned mark.)
                continue
            } catch (e: Exception) {
                // No network, or no longer signed in: every batch after this
                // one would fail the same way.
                break
            }
            lock.withLock { record(batch, result) }
            val edits = batch.filter { it.deleted != true }.associateBy { it.id }
            for (outcome in result.marks) {
                val edit = edits[outcome.id]?.takeIf { outcome.status == "stale" } ?: continue
                stale.getOrPut(edit.bookFileId) { mutableSetOf() } += edit.id
            }
        }

        // Stale: the server kept a version made later than ours. No pull will
        // bring it - the row did not change - so it is fetched here, or this
        // phone would go on showing the version that lost.
        for ((bookFileId, ids) in stale) {
            val listed = try {
                client.annotations(bookFileId)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                continue
            }
            applyListed(bookFileId, listed.items, replacing = ids)
        }
        return lock.withLock { pendingLocked().isEmpty() }
    }

    /**
     * Takes in what a sync pull said changed: this person's own marks, made
     * or edited elsewhere, and the ones deleted elsewhere.
     *
     * A local edit not yet sent is never replaced by a pull: the push decides
     * between the two, by the rule the server applies to it, and a push the
     * server calls stale fetches the version that won. A deletion wins over
     * any edit, sent or not, as the server rules: it would refuse to bring the
     * mark back, and keeping it here would leave a mark on this phone that no
     * other device will ever see.
     */
    suspend fun applyPulled(annotations: List<Annotation>, deleted: List<DeletedMark>) = lock.withLock {
        if (closed || (annotations.isEmpty() && deleted.isEmpty())) return@withLock
        val all = load()
        val touched = mutableMapOf<String, Held>()
        fun current(bookFileId: String) = touched[bookFileId] ?: all[bookFileId] ?: Held(bookFileId)

        for (a in annotations) {
            val h = current(a.bookFileId)
            touched[a.bookFileId] = h.merge(a) ?: continue
        }
        for (d in deleted) {
            val h = current(d.bookFileId)
            if (h.marks.none { it.id == d.id && !it.shared } && h.tombstones.none { it.id == d.id }) continue
            touched[d.bookFileId] = h.copy(
                marks = h.marks.filter { it.id != d.id || it.shared },
                // The server has the deletion already; ours has nothing left to say.
                tombstones = h.tombstones.filter { it.id != d.id },
            )
        }
        for (h in touched.values) save(all, h)
    }

    /**
     * Takes in every mark the reader may see in one BookFile, as the server
     * lists them when the book is opened.
     *
     * That list is the only place shared marks come from - the sync pull is
     * this person's own marks alone - so the shared ones here REPLACE the
     * shared ones held: a share that was withdrawn takes its highlights with
     * it. Own marks are merged exactly as a pull merges them.
     *
     * `replacing` names own marks the server just called stale: its copy
     * replaces this device's outright, and one it no longer lists at all was
     * deleted elsewhere.
     */
    suspend fun applyListed(bookFileId: String, items: List<Annotation>, replacing: Set<String> = emptySet()) = lock.withLock {
        if (closed) return@withLock
        val all = load()
        var h = all[bookFileId] ?: Held(bookFileId)
        val own = items.filter { it.shared != true && it.bookFileId == bookFileId }
        for (a in own) {
            h = h.merge(a, force = a.id in replacing) ?: h
        }
        val listed = own.map { it.id }.toSet()
        val gone = replacing - listed
        val shared = items.filter { it.shared == true && it.bookFileId == bookFileId }.map { it.toMark(Delivery.SENT) }
        h = h.copy(
            marks = h.marks.filter { !it.shared && !(it.id in gone && it.delivery == Delivery.SENT) } + shared,
        )
        save(all, h)
    }

    /**
     * Records what the server now knows about which own marks could not be
     * placed. Only the flag moves: not the version, not whether it was sent -
     * this is a fact about the document, reported on its own endpoint, and no
     * edit of anybody's.
     */
    suspend fun setOrphaned(bookFileId: String, orphaned: Set<String>, resolved: Set<String>) = lock.withLock {
        if (closed || (orphaned.isEmpty() && resolved.isEmpty())) return@withLock
        val all = load()
        val h = all[bookFileId] ?: return@withLock
        var changed = false
        val marks = h.marks.map { m ->
            val flag = when {
                m.shared -> m.orphaned
                m.id in orphaned -> true
                m.id in resolved -> false
                else -> m.orphaned
            }
            if (flag != m.orphaned) changed = true
            m.copy(orphaned = flag)
        }
        if (changed) save(all, h.copy(marks = marks))
    }

    /** Empties the store and closes it to writes until [reopen]. */
    suspend fun clear() = lock.withLock {
        closed = true
        held = null
        withContext(io) { dir.deleteRecursively() }
    }

    /** Opens the store to the account now signed in. */
    suspend fun reopen() = lock.withLock {
        closed = false
        held = null
    }

    /**
     * The mark as the server has it, over whatever this device holds - or
     * null to leave it be.
     *
     * Never over an unsent edit (the push decides), and over a sent one only
     * when the server's is not the older version - unless `force` - since a
     * list read a moment before this device's last push landed must not put
     * back what it replaced.
     */
    private fun Held.merge(a: Annotation, force: Boolean = false): Held? {
        if (a.shared == true) return null
        // Deleted here and not yet told: the deletion wins, as it will on the server.
        if (tombstones.any { it.id == a.id }) return null
        val local = marks.firstOrNull { it.id == a.id }
        if (local?.shared == true) return null
        // Never over an unsent edit - not even the version that just won a
        // stale push. An edit made while that push was in the air is newer
        // than both, and has not been sent: replacing it would lose it.
        if (local?.delivery == Delivery.UNSENT) return null
        // `force` answers a stale push, whose version the server has just said
        // won, whatever the clocks say. It skips this comparison and nothing else.
        if (local != null && !force) {
            val ours = instantOf(local.updatedAt)
            val theirs = instantOf(a.madeAt())
            if (ours != null && theirs != null && theirs.isBefore(ours)) return null
        }
        return copy(marks = marks.filter { it.id != a.id } + a.toMark(Delivery.SENT))
    }

    private suspend fun pendingLocked(): List<PushedMark> = load().values.flatMap { h ->
        h.marks.filter { it.delivery == Delivery.UNSENT && !it.shared }.map { it.pushed() } +
            h.tombstones.filter { it.delivery == Delivery.UNSENT }.map {
                PushedMark(id = it.id, bookFileId = it.bookFileId, updatedAt = it.deletedAt, deleted = true)
            }
    }

    /** What the server said about each change, for the changes still as they were sent. */
    private suspend fun record(batch: List<PushedMark>, result: SyncPushResult) {
        if (closed) return
        val all = load()
        val sent = batch.associateBy { it.id }
        val touched = mutableMapOf<String, Held>()
        for (outcome in result.marks) {
            val pushed = sent[outcome.id] ?: continue
            val h = touched[pushed.bookFileId] ?: all[pushed.bookFileId] ?: continue
            val delivery = when (outcome.status) {
                // Stale: the server holds a version made later, which flush()
                // fetches. This one is done either way.
                "applied", "stale" -> Delivery.SENT
                "refused" -> Delivery.REFUSED
                else -> continue
            }
            touched[pushed.bookFileId] = if (pushed.deleted == true) {
                h.copy(
                    tombstones = h.tombstones.mapNotNull { t ->
                        when {
                            t.id != pushed.id || t.deletedAt != pushed.updatedAt -> t
                            // Sent: the server keeps its own record of the deletion.
                            delivery == Delivery.SENT -> null
                            // Refused: kept, so a pull cannot bring the mark back.
                            else -> t.copy(delivery = delivery)
                        }
                    },
                )
            } else {
                h.copy(
                    marks = h.marks.map { m ->
                        if (m.id == pushed.id && m.delivery == Delivery.UNSENT && m.updatedAt == pushed.updatedAt) {
                            m.copy(delivery = delivery)
                        } else {
                            m
                        }
                    },
                )
            }
        }
        for (h in touched.values) save(all, h)
    }

    private suspend fun load(): MutableMap<String, Held> {
        held?.let { return it }
        val read = withContext(io) {
            dir.listFiles { f -> f.isFile && f.name.endsWith(".json") }.orEmpty().mapNotNull { f ->
                runCatching { json.decodeFromString<Held>(f.readText()) }.getOrElse { e ->
                    // Moved aside, not skipped: skipped, the next mark made in
                    // that book would write over the file and whatever unsent
                    // notes it held. Aside, they can still be recovered.
                    val aside = File(dir, "${f.name}.corrupt-${System.currentTimeMillis()}")
                    val moved = f.renameTo(aside)
                    log.warning("marks file ${f.name} could not be read (${e.message}); moved aside: $moved")
                    null
                }
            }
        }
        return read.associateByTo(mutableMapOf()) { it.bookFileId }.also { held = it }
    }

    private suspend fun save(all: MutableMap<String, Held>, h: Held) {
        if (closed) return
        all[h.bookFileId] = h
        withContext(io) {
            // Named by a hash: the id came from a server, and a name that came
            // from outside is never trusted as a path.
            val file = File(dir, hashed(h.bookFileId) + ".json")
            if (h.marks.isEmpty() && h.tombstones.isEmpty()) {
                file.delete()
            } else {
                file.writeAtomically(json.encodeToString(h).toByteArray())
            }
        }
    }

    private fun batches(items: List<PushedMark>): List<List<PushedMark>> {
        val out = mutableListOf<List<PushedMark>>()
        var batch = mutableListOf<PushedMark>()
        var bytes = 0
        for (item in items) {
            val size = json.encodeToString(item).toByteArray().size
            if (batch.isNotEmpty() && (batch.size >= BATCH_ITEMS || bytes + size > BATCH_BYTES)) {
                out += batch
                batch = mutableListOf()
                bytes = 0
            }
            batch += item
            bytes += size
        }
        if (batch.isNotEmpty()) out += batch
        return out
    }

    companion object {
        private val log: Logger = Logger.getLogger("lexarii.marks")

        // Well under the server's 500 changes and 2 MiB per push.
        private const val BATCH_ITEMS = 100
        private const val BATCH_BYTES = 512 * 1024

        /**
         * A server timestamp, which may carry an offset rather than a Z - the
         * server writes its own zone - or null if it is not one.
         */
        internal fun instantOf(value: String?): Instant? =
            value?.let { runCatching { OffsetDateTime.parse(it).toInstant() }.getOrNull() }
    }
}

private fun Mark.pushed() = PushedMark(
    id = id,
    bookFileId = bookFileId,
    type = type,
    start = start,
    end = end,
    selectedText = selectedText,
    note = note,
    colour = colour,
    updatedAt = updatedAt,
)

/**
 * When the server's version was made, by whichever device made it - the time
 * its last-write-wins compares. Not `updatedAt`, which is when the server
 * stored it: two edits made a minute apart can arrive hours apart. The server
 * leaves `editedAt` out only when it does not know it, and then when it stored
 * the mark is the best there is.
 */
private fun Annotation.madeAt(): String = editedAt ?: updatedAt

private fun Annotation.toMark(delivery: Delivery) = Mark(
    id = id,
    bookFileId = bookFileId,
    type = type,
    start = start,
    end = end,
    selectedText = selectedText,
    note = note,
    colour = colour,
    orphaned = orphaned,
    shared = shared == true,
    author = author,
    createdAt = createdAt,
    updatedAt = madeAt(),
    delivery = delivery,
)
