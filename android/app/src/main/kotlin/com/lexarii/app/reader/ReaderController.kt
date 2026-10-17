// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import com.lexarii.app.api.Asset
import com.lexarii.app.api.PushedPosition
import com.lexarii.app.api.LexariiClient
import com.lexarii.app.offline.Delivery
import com.lexarii.app.offline.Mark
import com.lexarii.app.offline.MarkStore
import com.lexarii.app.offline.OfflineStore
import com.lexarii.app.offline.Outbox
import com.lexarii.app.offline.Sync
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import java.time.Instant
import java.util.UUID

/** What the reader screen is showing. */
sealed interface ReaderState {
    data object Opening : ReaderState
    data class Failed(val message: String) : ReaderState

    /** A PDF, for now. Said plainly rather than shown as a blank page. */
    data class Unsupported(val format: String) : ReaderState

    /**
     * One chapter, and where in it to put the reader.
     *
     * `generation` changes on every new instruction, so showing the same
     * chapter at the same target twice - "take me back to where I was" - is
     * still a change the screen acts on.
     */
    data class Showing(
        val html: String,
        val spineIndex: Int,
        val spineCount: Int,
        val target: Target,
        val generation: Int,
    ) : ReaderState
}

/** Where in a chapter to open it. */
sealed interface Target {
    data object Start : Target

    /** Turning back from the chapter after lands on this one's last page. */
    data object End : Target
    data class Path(val path: String) : Target
    data class Fragment(val id: String) : Target
}

/** The mark whose sheet is open, and whether it was opened to write its note. */
data class OpenMark(val id: String, val writing: Boolean)

/**
 * A note somebody was writing when its mark was deleted on another device.
 * The deletion wins, as it does everywhere; the words are kept to be saved
 * again on the same passage, or copied, or let go - never silently lost.
 */
data class LostNote(val start: String, val end: String, val selectedText: String?, val text: String)

/** A selection the page handed over, checked: where it runs and what it said. */
data class Anchor(val start: String, val end: String, val selectedText: String)

/**
 * One open BookFile: which chapter is on screen, and where the reader is.
 *
 * The WebView lays out and reports; this decides and remembers. Keeping the
 * spine and the position here rather than in the page is what lets a rotation
 * throw the WebView away and put the reader back on the same paragraph.
 *
 * Chapters and images come from the copy kept on the device when there is
 * one, and from the server otherwise - the same responses either way.
 *
 * The position goes to the server through the sync push, not the web reader's
 * progress endpoint, because this is the client the sync protocol was built
 * for: the time on it is this device's own, so a position recorded on a train
 * and sent an hour later still lands as an hour old. It goes through the
 * outbox, on disk before it is sent, so it survives the app being killed.
 *
 * Highlights and notes go the same way, through the mark store: on disk
 * first, then pushed with an id made here and this device's own time.
 */
class ReaderController(
    val bookFileId: String,
    private val client: LexariiClient,
    private val kept: OfflineStore,
    private val outbox: Outbox,
    private val store: MarkStore,
    private val sync: Sync,
    private val scope: CoroutineScope,
    private val clock: () -> Instant = Instant::now,
    private val newId: () -> String = { UUID.randomUUID().toString() },
) {
    private val _state = MutableStateFlow<ReaderState>(ReaderState.Opening)
    val state: StateFlow<ReaderState> = _state.asStateFlow()

    /** Every mark in this BookFile, own and shared, for the page to paint the chapter's share of. */
    private val _marks = MutableStateFlow<List<Mark>>(emptyList())
    val marks: StateFlow<List<Mark>> = _marks.asStateFlow()

    private val _openMark = MutableStateFlow<OpenMark?>(null)
    val openMark: StateFlow<OpenMark?> = _openMark.asStateFlow()

    /**
     * How many marks in the chapter on screen the page could not find.
     *
     * Counted and said, never acted on: an anchor that stopped resolving is
     * still somebody's note, and the web shows it as orphaned rather than
     * losing it.
     */
    private val _unplaced = MutableStateFlow(0)
    val unplaced: StateFlow<Int> = _unplaced.asStateFlow()

    private var refresh: Job? = null

    /**
     * Something the reader asked for that did not happen, said once. A mark
     * that could not be kept must not simply fail to appear: somebody would
     * go on reading believing it was there.
     */
    private val _notice = MutableStateFlow<String?>(null)
    val notice: StateFlow<String?> = _notice.asStateFlow()

    fun noticeSeen() {
        _notice.value = null
    }

    private val _lostNote = MutableStateFlow<LostNote?>(null)
    val lostNote: StateFlow<LostNote?> = _lostNote.asStateFlow()

    /** What the open sheet has typed and not saved, by mark: null for nothing typed. */
    private var typing: Pair<String, String>? = null

    private var spineCount = 0
    private var spineIndex = 0
    private var generation = 0
    private var load: Job? = null

    /** The last position the page reported, so a new WebView can be put back on it. */
    private var here: EpubLocator? = null

    /** What to retry if a load fails. */
    private var lastRequest: (() -> Unit)? = null

    fun open() {
        lastRequest = ::open
        _state.value = ReaderState.Opening
        refresh?.cancel()
        // Beside the chapter, not before it: the page paints marks whenever
        // they arrive, so nobody waits on them to start reading.
        refresh = scope.launch { refreshMarks() }
        load?.cancel()
        load = scope.launch {
            try {
                val manifest = kept.manifest(bookFileId) ?: client.manifest(bookFileId)
                if (manifest.format != "epub" || manifest.spine.isEmpty()) {
                    _state.value = ReaderState.Unsupported(manifest.format)
                    return@launch
                }
                spineCount = manifest.spine.size

                val start = startingPoint()?.takeIf { it.spineIndex in 0 until spineCount }
                if (start != null) {
                    showNow(start.spineIndex, Target.Path(start.path))
                } else {
                    showNow(0, Target.Start)
                }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                _state.value = ReaderState.Failed(e.message ?: "Could not open this book")
            }
        }
    }

    /**
     * Where to open: the newer of this device's own last position and the
     * server's, by when each was read.
     *
     * Somewhere to start is not worth failing over, and not worth waiting on
     * either when the chapters are already here: a kept copy opened with no
     * network gets a few seconds for the server to answer, then opens where
     * this device last was.
     */
    private suspend fun startingPoint(): EpubLocator? =
        EpubLocator.parse(resumeFrom(outbox.last(bookFileId), client, bookFileId, kept.has(bookFileId), SERVER_PATIENCE_MS))

    /** An image a chapter asked for: from the kept copy if it has it. */
    suspend fun asset(path: String): Asset = kept.asset(bookFileId, path) ?: client.asset(bookFileId, path)

    fun retry() {
        lastRequest?.invoke()
    }

    /** The page ran off one end of its chapter. Past either end of the book, nothing happens. */
    fun turnedOffEdge(direction: Int) {
        val next = spineIndex + direction.coerceIn(-1, 1)
        if (next !in 0 until spineCount || _state.value !is ReaderState.Showing) return
        show(next, if (direction < 0) Target.End else Target.Start)
    }

    /** A link inside the book. The page cannot navigate; it asks. */
    fun follow(spine: Int, fragment: String) {
        if (spine !in 0 until spineCount) return
        show(spine, if (fragment.isNotEmpty()) Target.Fragment(fragment) else Target.Start)
    }

    /**
     * The page says where the reader is.
     *
     * Checked, because it came out of a WebView that holds untrusted HTML:
     * anything that is not a well-formed position in the chapter on screen is
     * dropped rather than sent to the server as this person's place.
     */
    fun reportPosition(locator: String, percent: Double) {
        val parsed = EpubLocator.parse(locator) ?: return
        if (parsed.spineIndex != spineIndex) return
        here = parsed
        val position = PushedPosition(
            bookFileId = bookFileId,
            position = locator,
            percent = percent.takeIf { it.isFinite() }?.coerceIn(0.0, 1.0),
            readAt = clock().toString(),
        )
        scope.launch {
            outbox.put(position)
            outbox.flush(client)
        }
    }

    /**
     * A new WebView, after a rotation. The chapter on screen is shown again,
     * at the paragraph last reported rather than wherever it was first opened.
     */
    fun reattached() {
        val showing = _state.value as? ReaderState.Showing ?: return
        val at = here?.takeIf { it.spineIndex == showing.spineIndex } ?: return
        _state.value = showing.copy(target = Target.Path(at.path), generation = ++generation)
    }

    /** Sends whatever is still unsent. Called on the way out of the reader. */
    suspend fun send() {
        outbox.flush(client)
        store.flush(client)
    }

    /**
     * The page's answer to `Reader.highlightSelection()`, made into a
     * highlight - and with its note open for writing when that is what was
     * asked for. False if the answer was not a usable selection.
     *
     * Always a highlight, as the web makes it. A note with no body is refused
     * by the server, and there is no body yet; writing one makes it a note.
     */
    fun highlight(selection: String?, writing: Boolean = false): Boolean {
        val anchor = anchorOf(selection)
        if (anchor == null) {
            _notice.value = "That selection could not be highlighted."
            return false
        }
        val now = clock().toString()
        val mark = Mark(
            id = newId(),
            bookFileId = bookFileId,
            type = TYPE_HIGHLIGHT,
            start = anchor.start,
            end = anchor.end,
            selectedText = anchor.selectedText,
            createdAt = now,
            updatedAt = now,
            delivery = Delivery.UNSENT,
        )
        scope.launch {
            if (!store.put(mark)) {
                _notice.value = "That highlight could not be saved."
                return@launch
            }
            reloadMarks()
            if (writing) _openMark.value = OpenMark(mark.id, writing = true)
            store.flush(client)
            reloadMarks()
        }
        return true
    }

    /**
     * What the page handed over as a selection, or null if it is not one.
     *
     * The page is a WebView holding untrusted HTML, so nothing it says is
     * taken on trust: both ends must be well-formed positions in the chapter
     * on screen, running forwards within one element, around some text.
     */
    internal fun anchorOf(raw: String?): Anchor? {
        val showing = _state.value as? ReaderState.Showing ?: return null
        var element = parse(raw) ?: return null
        // The object itself, as evaluateJavascript hands back a returned
        // object - or, should the page ever send it as a string, the object
        // inside that.
        if (element is JsonPrimitive && element.isString) element = parse(element.content) ?: return null
        val fields = element as? JsonObject ?: return null

        val start = fields.text("start")?.takeIf { it.length <= MAX_LOCATOR }?.let(EpubLocator::parse) ?: return null
        val end = fields.text("end")?.takeIf { it.length <= MAX_LOCATOR }?.let(EpubLocator::parse) ?: return null
        if (start.spineIndex != showing.spineIndex || end.spineIndex != showing.spineIndex) return null
        if (start.path == end.path && start.charOffset >= end.charOffset) return null

        val text = fields.text("selectedText")?.trim()?.takeIf { it.isNotEmpty() } ?: return null
        return Anchor(start.format(), end.format(), text.capped(MAX_SELECTED_TEXT))
    }

    /** The page says a painted mark was tapped. Only a mark this reader holds opens. */
    fun markTapped(id: String) {
        if (_marks.value.any { it.id == id }) _openMark.value = OpenMark(id, writing = false)
    }

    /**
     * The page could not find these marks in the chapter on screen. Counted,
     * never dropped - and told to the server, as the web tells it, so a note
     * whose passage has gone is flagged everywhere rather than just missing.
     */
    fun unresolved(idsJson: String, spineIndex: Int) {
        // A report from the chapter before, arriving after the next was put
        // on screen, says nothing about this one - and judged against this
        // one's marks it would clear flags it never looked at.
        if ((_state.value as? ReaderState.Showing)?.spineIndex != spineIndex) return
        val ids = unresolvedIds(idsJson) ?: return
        val held = _marks.value.map { it.id }.toSet()
        _unplaced.value = ids.count { it in held }
        lastReport = spineIndex to ids
        reportOrphans(ids, spineIndex)
    }

    /**
     * The chapter on screen's last report. The page paints - and reports -
     * before the marks have come back from the server, and does not report
     * again when they come if its list is the same: judged only once, a flag
     * that arrived with them would never be cleared. So the report is judged
     * again whenever the marks held change.
     */
    private var lastReport: Pair<Int, Set<String>>? = null

    /**
     * The same from a page that does not say which chapter it painted: an
     * older reader.js. Counted for the chapter on screen, but not reported -
     * without the chapter, a report cannot be told apart from a late one.
     */
    fun unresolved(idsJson: String) {
        val ids = unresolvedIds(idsJson) ?: return
        val held = _marks.value.map { it.id }.toSet()
        _unplaced.value = ids.count { it in held }
    }

    private fun unresolvedIds(idsJson: String): Set<String>? =
        (parse(idsJson) as? JsonArray)
            ?.mapNotNull { (it as? JsonPrimitive)?.takeIf { p -> p.isString }?.content }
            ?.toSet()

    /**
     * Flags this reader's own marks in the chapter on screen that the page
     * could not place, and clears the flag from ones it now could - the web's
     * rule, so a passage that comes back after a re-ingest does not leave its
     * note flagged for good.
     *
     * Only marks the server has: one not sent yet has no row to flag, and a
     * shared one is somebody else's, which the server would not flag for this
     * reader anyway. Best effort and online only. Nothing is queued when the
     * server cannot be reached - the next time the chapter is painted online
     * says the same thing - and nothing is ever deleted, here or there.
     *
     * The page reports after every paint of a chapter, an empty list
     * included, so a chapter where everything is placed is what clears a flag.
     * "Placed" is judged against the marks held when the answer arrives; one
     * that arrived in between, after the page had painted, can be taken for
     * placed, and the paint that follows it reports again and puts that right.
     */
    private fun reportOrphans(unplaced: Set<String>, chapter: Int) {
        val own = _marks.value.filter {
            !it.shared && it.delivery == Delivery.SENT && EpubLocator.parse(it.start)?.spineIndex == chapter
        }
        val flag = own.filter { it.id in unplaced && !it.orphaned }.map { it.id }
        val clear = own.filter { it.id !in unplaced && it.orphaned }.map { it.id }
        if (flag.isEmpty() && clear.isEmpty()) return
        scope.launch {
            try {
                client.reportOrphans(bookFileId, flag, clear)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                return@launch
            }
            store.setOrphaned(bookFileId, flag.toSet(), clear.toSet())
            reloadMarks()
        }
    }

    fun closeMark() {
        _openMark.value = null
        typing = null
    }

    /**
     * The open sheet's unsaved words, as they change: null when nothing has
     * been typed. Held so that a deletion arriving from another device while
     * somebody writes can hand their words back rather than take them.
     */
    fun drafting(id: String, typed: String?) {
        typing = if (typed != null && _openMark.value?.id == id) id to typed else null
    }

    /** Saves a lost note's words as a new note on the same passage. */
    fun keepLostNote() {
        val lost = _lostNote.value ?: return
        _lostNote.value = null
        val body = lost.text.trim().capped(MAX_NOTE).takeIf { it.isNotEmpty() } ?: return
        val now = clock().toString()
        val mark = Mark(
            id = newId(),
            bookFileId = bookFileId,
            type = TYPE_NOTE,
            start = lost.start,
            end = lost.end,
            selectedText = lost.selectedText,
            note = body,
            createdAt = now,
            updatedAt = now,
            delivery = Delivery.UNSENT,
        )
        scope.launch {
            if (!store.put(mark)) {
                _notice.value = "That note could not be saved."
                return@launch
            }
            reloadMarks()
            store.flush(client)
            reloadMarks()
        }
    }

    fun dropLostNote() {
        _lostNote.value = null
    }

    /**
     * Writes, changes, or clears the note on one of this person's own marks.
     *
     * The type follows the note, exactly as the server's own edit decides it:
     * a note body makes a note, clearing it makes a highlight again if there
     * is text to highlight, and anything else keeps what it was. The server's
     * push takes the type as sent, so this is where that rule has to live.
     */
    fun updateNote(id: String, note: String) {
        val mark = _marks.value.firstOrNull { it.id == id && !it.shared } ?: return
        _openMark.value = null
        typing = null
        val body = note.trim().capped(MAX_NOTE).takeIf { it.isNotEmpty() }
        if (body == mark.note) return
        // A note with no text under it stays a note when its body is cleared,
        // and the server refuses a note with no body: this phone would show it
        // cleared while the server kept refusing it. Removing such a note is
        // deleting it, which the sheet offers as that.
        if (body == null && !canClearNote(mark)) return
        val type = when {
            body != null -> TYPE_NOTE
            mark.selectedText != null -> TYPE_HIGHLIGHT
            else -> mark.type
        }
        val edited = mark.copy(note = body, type = type, updatedAt = editedAt(mark.updatedAt), delivery = Delivery.UNSENT)
        scope.launch {
            if (!store.put(edited)) {
                _notice.value = "That note could not be saved."
                return@launch
            }
            reloadMarks()
            store.flush(client)
            reloadMarks()
        }
    }

    /** Whether clearing this mark's note leaves something the server will keep. */
    fun canClearNote(mark: Mark): Boolean = mark.selectedText != null || mark.type != TYPE_NOTE

    /** Deletes one of this person's own marks. Somebody else's shared mark is not theirs to delete. */
    fun delete(id: String) {
        if (_marks.value.none { it.id == id && !it.shared }) return
        _openMark.value = null
        typing = null
        scope.launch {
            if (!store.delete(bookFileId, id, clock().toString())) {
                _notice.value = "That could not be deleted."
                return@launch
            }
            reloadMarks()
            store.flush(client)
        }
    }

    /**
     * This device's time for an edit, but always after the version edited.
     *
     * The server keeps whichever version was made last, and the version on
     * screen may carry the server's clock. A phone a minute behind it would
     * otherwise make an edit the server calls older than what it replaces,
     * and throw away.
     */
    private fun editedAt(previous: String): String {
        val now = clock()
        val before = MarkStore.instantOf(previous) ?: return now.toString()
        return if (now.isAfter(before)) now.toString() else before.plusMillis(1).toString()
    }

    /**
     * Brings this BookFile's marks up to date: sends what this device made,
     * pulls what changed elsewhere, and asks for the marks shared with this
     * reader, which only the per-book list carries.
     *
     * Best effort, and bounded: a kept copy opened with no network shows the
     * marks this device already has.
     */
    private suspend fun refreshMarks() {
        reloadMarks()
        withTimeoutOrNull(SERVER_PATIENCE_MS * 3) {
            quietly { store.flush(client) }
            quietly { sync.pull(client) }
            quietly { store.applyListed(bookFileId, client.annotations(bookFileId).items) }
        }
        reloadMarks()
    }

    private suspend fun reloadMarks() {
        val before = _marks.value
        _marks.value = store.marks(bookFileId).sortedBy { it.createdAt }
        lastReport?.let { (chapter, ids) ->
            if (_marks.value != before && (_state.value as? ReaderState.Showing)?.spineIndex == chapter) reportOrphans(ids, chapter)
        }
        // The open mark is gone: deleted on another device, or a share that
        // was withdrawn. The sheet cannot stay open on nothing, and must not
        // just vanish either.
        val open = _openMark.value ?: return
        if (_marks.value.any { it.id == open.id }) return
        val gone = before.firstOrNull { it.id == open.id }
        val typed = typing?.takeIf { it.first == open.id }?.second
        _openMark.value = null
        typing = null
        if (gone == null) return
        if (gone.shared) {
            _notice.value = "This note is no longer shared with you."
            return
        }
        _notice.value = "This note was deleted on another device."
        if (typed != null && typed.isNotBlank()) {
            _lostNote.value = LostNote(gone.start, gone.end, gone.selectedText, typed)
        }
    }

    private fun show(index: Int, target: Target) {
        lastRequest = { show(index, target) }
        load?.cancel()
        load = scope.launch {
            try {
                showNow(index, target)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                _state.value = ReaderState.Failed(e.message ?: "Could not load that chapter")
            }
        }
    }

    private suspend fun showNow(index: Int, target: Target) {
        val content = kept.spine(bookFileId, index) ?: client.spine(bookFileId, index)
        spineIndex = index
        // The page reports this chapter's own when it has painted it.
        _unplaced.value = 0
        lastReport = null
        _state.value = ReaderState.Showing(content.html, index, spineCount, target, ++generation)
    }

    companion object {
        private const val SERVER_PATIENCE_MS = 3_000L

        private const val TYPE_HIGHLIGHT = "highlight"
        private const val TYPE_NOTE = "note"

        // The server bounds none of these on their own, only the push they
        // travel in. These keep one mark far inside that, and still hold any
        // passage anybody means to highlight, or any note typed on a phone.
        private const val MAX_LOCATOR = 4_096
        internal const val MAX_SELECTED_TEXT = 10_000
        /** The longest note kept; the editor stops there too. */
        const val MAX_NOTE = 32_000

        private fun parse(raw: String?): JsonElement? = raw?.let { runCatching { Json.parseToJsonElement(it) }.getOrNull() }

        private fun JsonObject.text(key: String): String? = (this[key] as? JsonPrimitive)?.takeIf { it.isString }?.content

        /** At most `limit` UTF-16 units, never cutting a character in half. */
        private fun String.capped(limit: Int): String {
            if (length <= limit) return this
            val cut = if (Character.isHighSurrogate(this[limit - 1])) limit - 1 else limit
            return substring(0, cut)
        }

        private suspend fun quietly(block: suspend () -> Unit) {
            try {
                block()
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                // Best effort; the marks already here are what is shown.
            }
        }
    }
}
