// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app

import android.app.Application
import androidx.compose.ui.graphics.ImageBitmap
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.initializer
import androidx.lifecycle.viewmodel.viewModelFactory
import com.lexarii.app.api.AuthToken
import com.lexarii.app.api.DataStoreTokenStore
import com.lexarii.app.api.InMemoryTokenStore
import com.lexarii.app.api.LibraryItem
import com.lexarii.app.api.NotSignedIn
import com.lexarii.app.api.ServerRefused
import com.lexarii.app.api.LexariiClient
import com.lexarii.app.reader.PdfController
import com.lexarii.app.reader.ReaderController
import com.lexarii.app.reader.ReadingSettings
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull

/** What the one screen can be showing. */
sealed interface UiState {
    /**
     * The form, with what it should be filled in with.
     *
     * The username rides along because the form leaves the screen while a
     * sign-in is in flight and comes back empty otherwise - a mistyped
     * password should not cost somebody their username too.
     */
    data class SignedOut(val server: String = "", val error: String? = null, val username: String = "") : UiState
    data object Loading : UiState

    /**
     * Sending what can still be sent, then emptying the device. Its own
     * screen, so nothing on the shelf - another sign-out, a book - can be
     * tapped while it runs.
     */
    data object SigningOut : UiState

    /**
     * Signed in, but the server did not answer and there is no shelf saved
     * from an earlier run to show instead.
     *
     * Not SignedOut: the token is still good, and a phone on a train or a
     * server being restarted must not cost somebody their password.
     */
    data class Unreachable(val server: String, val error: String) : UiState

    /**
     * The shelf. `offline` is why it is the one saved from the last run
     * rather than the server's - and then only the copies kept on the device
     * can be opened from it.
     *
     * `complete` is false while more of the library is still to come from the
     * server; `failed` says why the next page did not.
     */
    data class Library(
        val items: List<LibraryItem>,
        val syncedAt: String?,
        val offline: String? = null,
        val complete: Boolean = true,
        val loadingMore: Boolean = false,
        val failed: String? = null,
    ) : UiState
}

/**
 * What signing out would throw away, asked about before it does. Refused
 * marks are counted apart: they are not waiting to be sent, they exist on
 * this device and nowhere else.
 */
data class SignOutCost(val kept: Int, val unsent: Int, val unsentMarks: Int = 0, val refusedMarks: Int = 0)

/** The reader over the shelf, once it knows what kind of book it is opening. */
sealed interface Reading {
    data object Opening : Reading
    data class CannotOpen(val message: String) : Reading
    class Epub(val controller: ReaderController, val server: String) : Reading
    class Pdf(val controller: PdfController) : Reading
}

class LibraryViewModel(application: Application) : AndroidViewModel(application) {
    private val tokens = DataStoreTokenStore(application)
    private val device = Services.device(application)
    private val queue = KeepQueue(application)
    private val settings = Settings(application)
    private val _state = MutableStateFlow<UiState>(UiState.Loading)
    val state: StateFlow<UiState> = _state.asStateFlow()

    private var client: LexariiClient? = null
    private var server: String = ""
    private var pager: ShelfPager? = null

    /**
     * Everything done for the signed-in account - loading the library, the
     * open reader - runs here, so leaving the account can stop all of it and
     * WAIT until it has stopped before the device is cleared.
     *
     * Without the wait, a pull still coming back over a slow network lands
     * after the clear: the last person's notes and sync cursor are written
     * into an empty account directory, and whoever signs in next inherits
     * them as their own.
     */
    private var account = accountScope()

    private fun accountScope() = CoroutineScope(viewModelScope.coroutineContext + SupervisorJob(viewModelScope.coroutineContext[Job]))

    /** Whose turn the device is, between signing in and signing out. */
    private val turns = AccountTurns()

    /** Stops everything the last account had in flight, and returns once it has. */
    private suspend fun endAccount() {
        val leaving = account
        account = accountScope()
        leaving.coroutineContext[Job]?.cancelAndJoin()
    }

    /**
     * The open reader, over the library rather than instead of it: closing it
     * goes back to the shelf as it was, without reloading anything.
     */
    private val _reading = MutableStateFlow<Reading?>(null)
    val reading: StateFlow<Reading?> = _reading.asStateFlow()
    private var opening: Job? = null

    /** Copies complete on the disk. The work queue says what is on its way. */
    private val kept = MutableStateFlow(emptySet<String>())

    val keeping: StateFlow<Map<String, Keeping>> = combine(kept, queue.states) { onDisk, queued ->
        // What is on the disk is kept, whatever an old finished piece of work
        // says; what is queued or running is said as that.
        queued.filterValues { it != Keeping.Kept } + onDisk.associateWith { Keeping.Kept }
    }.stateIn(viewModelScope, SharingStarted.Eagerly, emptyMap())

    private val _confirmSignOut = MutableStateFlow<SignOutCost?>(null)
    val confirmSignOut: StateFlow<SignOutCost?> = _confirmSignOut.asStateFlow()

    /** How the reader lays out text, on this device. */
    val readingSettings: StateFlow<ReadingSettings> =
        settings.reading.stateIn(viewModelScope, SharingStarted.Eagerly, ReadingSettings())

    val unmeteredOnly: StateFlow<Boolean> = settings.unmeteredOnly.stateIn(viewModelScope, SharingStarted.Eagerly, false)

    val covers = Covers(device.covers) { client }

    init {
        viewModelScope.launch {
            kept.value = device.kept.kept()
            resume()
        }
        // A download finishing in the background, while the shelf is open,
        // becomes a kept copy on it.
        viewModelScope.launch {
            queue.states.collect { states ->
                if (states.values.any { it == Keeping.Kept }) kept.value = device.kept.kept()
            }
        }
    }

    /** Open where the last run left off: straight into the library if a token is held. */
    private suspend fun resume() {
        val saved = tokens.server()
        if (saved == null || tokens.token() == null) {
            _state.value = UiState.SignedOut(saved.orEmpty())
            return
        }
        val connection = LexariiClient(saved, tokens)
        client = connection
        server = saved
        account.launch { load(connection) }
    }

    fun open(item: LibraryItem) {
        val connection = client ?: return
        opening?.cancel()
        _reading.value = Reading.Opening
        opening = account.launch {
            val format = try {
                formatOf(connection, item)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                _reading.value = Reading.CannotOpen(e.message ?: "Could not open this book")
                return@launch
            }
            _reading.value = when (format) {
                "epub" -> Reading.Epub(
                    ReaderController(item.bookFileId, connection, device.kept, device.outbox, device.marks, device.sync, account)
                        .also { it.open() },
                    server,
                )
                "pdf" -> Reading.Pdf(
                    PdfController(item.bookFileId, connection, device.kept, device.pages, device.outbox, account).also { it.open() },
                )
                else -> Reading.CannotOpen("Reading ${format.uppercase()} files is not in the app yet. It opens in the browser reader.")
            }
        }
    }

    /**
     * What kind of file the listed BookFile is. A kept copy knows; a work
     * held in one format says so in the listing; one held in several has to
     * be asked, because the listing names the file, not its format.
     */
    private suspend fun formatOf(connection: LexariiClient, item: LibraryItem): String =
        device.kept.manifest(item.bookFileId)?.format
            ?: item.formats.singleOrNull()?.lowercase()
            ?: connection.info(item.bookFileId).format

    /** Back to the shelf. Whatever position or mark the reader has not sent yet goes now. */
    fun closeReader() {
        opening?.cancel()
        val open = _reading.value ?: return
        _reading.value = null
        when (open) {
            is Reading.Epub -> account.launch { open.controller.send() }
            is Reading.Pdf -> account.launch { open.controller.send() }
            else -> Unit
        }
    }

    fun saveReadingSettings(settings: ReadingSettings) {
        viewModelScope.launch { this@LibraryViewModel.settings.save(settings) }
    }

    fun setUnmeteredOnly(on: Boolean) {
        viewModelScope.launch {
            settings.setUnmeteredOnly(on)
            // Books already waiting take the new setting too, not only the
            // ones kept from now on.
            device.owner()?.let { queue.retarget(it, on) }
        }
    }

    /**
     * Keep a copy on the device, to read with no network.
     *
     * Only when the server says it may be downloaded: a book lent to be read
     * rather than kept is not quietly kept anyway, chapter by chapter.
     */
    fun keep(item: LibraryItem) {
        if (client == null || !item.canDownload) return
        viewModelScope.launch {
            // The account the book is kept for. The work checks it is still
            // the one signed in before it writes a byte.
            val owner = device.owner() ?: return@launch
            queue.keep(item.bookFileId, item.title, owner, settings.unmeteredOnly.first())
        }
    }

    /** Stop a download, or delete a kept copy. The book stays on the shelf either way. */
    fun discard(bookFileId: String) {
        queue.cancel(bookFileId)
        viewModelScope.launch {
            device.kept.remove(bookFileId)
            kept.value = device.kept.kept()
        }
    }

    fun retry() {
        val connection = client ?: return
        _state.value = UiState.Loading
        account.launch { load(connection) }
    }

    /**
     * The shelf is near its end, or somebody asked to try again: the rest of
     * the library, if any is due and none is on its way.
     */
    fun loadMore() {
        val current = pager ?: return
        val shelf = _state.value as? UiState.Library ?: return
        if (shelf.offline != null || shelf.complete || shelf.loadingMore) return
        account.launch { more(current, all = true) }
    }

    suspend fun cover(bookFileId: String): ImageBitmap? = covers.load(bookFileId)

    /**
     * Sign out, asking first if that throws something away.
     *
     * Signing out empties the device of this account: its kept copies, its
     * saved shelf, and any position or mark not yet sent. Whoever signs in next -
     * perhaps somebody else - must not find the last person's books on the
     * phone, or have their reading sent to the server as their own.
     */
    fun requestSignOut() {
        viewModelScope.launch {
            val cost = SignOutCost(
                kept = device.kept.kept().size,
                unsent = device.outbox.unsent().size,
                unsentMarks = device.marks.unsent(),
                refusedMarks = device.marks.refused(),
            )
            val nothingLost = cost.kept == 0 && cost.unsent == 0 && cost.unsentMarks == 0 && cost.refusedMarks == 0
            if (nothingLost) signOut() else _confirmSignOut.value = cost
        }
    }

    fun cancelSignOut() {
        _confirmSignOut.value = null
    }

    /**
     * Forget the token and go back to the form, with the address kept.
     *
     * Local only. The server-side session is revoked from the sessions list
     * on another device - this runs when the server may not be reachable at
     * all, which is exactly when somebody needs a way out.
     */
    fun signOut() {
        if (turns.signingOut) return
        _confirmSignOut.value = null
        _reading.value = null
        _state.value = UiState.SigningOut
        // Downloads run outside this screen, so they are stopped outside it:
        // nothing of this account's may be fetched after it has gone.
        queue.cancelAll()
        val connection = client
        client = null
        pager = null
        val address = server
        turns.leave(viewModelScope) { stillOurs ->
            // The token first, before anything can suspend long enough for a
            // sign-in to save another: this account's last send goes with its
            // own token, never the next person's.
            val own = tokens.frozen()
            // Nothing of this account may still be running when the device is
            // cleared, or it writes into the empty directory afterwards.
            endAccount()
            connection?.close()
            // One last try at the positions and marks, before they are thrown
            // away - bounded, because this runs exactly when the server may be
            // unreachable, and signing out is how somebody gets out of that.
            if (connection != null) {
                val last = LexariiClient(address, own)
                withTimeoutOrNull(LAST_SEND_MS) {
                    device.outbox.flush(last)
                    device.marks.flush(last)
                }
                last.close()
            }
            // Overtaken by a sign-in: the token and the device are theirs now.
            if (!stillOurs()) return@leave
            tokens.clear()
            device.clear()
            covers.forget()
            kept.value = emptySet()
            _state.value = UiState.SignedOut(address)
        }
    }

    fun signIn(server: String, username: String, password: String) {
        val address = server.trim()
        if (address.isEmpty()) {
            _state.value = UiState.SignedOut(server, "Where is your library?", username)
            return
        }

        _state.value = UiState.Loading
        viewModelScope.launch {
            // Before anything else: a sign-out still finishing goes first, and
            // from here on anything that begins after this - another sign-out,
            // another sign-in - makes `ours` false.
            val ours = turns.enter()
            // Its own token store, not the shared one: the token is saved for
            // the app only once this sign-in is known to still be the one that
            // counts, after the device has been claimed for it.
            val attempt = LexariiClient(address, InMemoryTokenStore())
            // One value for every way this can fail, so no branch can forget
            // to stop: a refused sign-in that fell through to loading would be
            // reported as the library's 401 instead of the password's.
            var issued: AuthToken? = null
            val refusal: String? = try {
                // The device name is required by the server, and is what the
                // person will be looking at in a year when they revoke this.
                issued = attempt.signIn(username, password, device = deviceName())
                null
            } catch (e: CancellationException) {
                throw e
            } catch (e: NotSignedIn) {
                e.message ?: "That username and password do not match"
            } catch (e: ServerRefused) {
                e.message ?: "The server refused the sign-in"
            } catch (e: Exception) {
                // No server at that address, no network, bad hostname. The
                // message is shown as-is because "failed" tells somebody
                // nothing about which of those it was.
                e.message ?: "Could not reach that server"
            } finally {
                attempt.close()
            }
            if (!ours()) return@launch
            val token = issued
            if (refusal != null || token == null) {
                _state.value = UiState.SignedOut(address, refusal, username)
                return@launch
            }
            // A token that expired or was revoked sends somebody back to this
            // form without signing out. If it is somebody else coming back -
            // another account, another server - what the last one left here
            // is not theirs, and neither is anything it still has running.
            endAccount()
            _reading.value = null
            if (device.claim("$address ${token.user.id}")) {
                queue.cancelAll()
                covers.forget()
                kept.value = emptySet()
            }
            tokens.save(token.token, address)
            val connection = LexariiClient(address, tokens)
            client?.close()
            client = connection
            this@LibraryViewModel.server = address
            account.launch { load(connection) }
        }
    }

    private suspend fun load(connection: LexariiClient) {
        val shelf = ShelfPager(connection, device.shelf)
        pager = shelf
        try {
            val first = shelf.first()
            // Marks made offline go first, so the pull after them brings back
            // the server's word on each rather than the version before.
            device.marks.flush(connection)
            // Everything that changed elsewhere since the last pull that
            // finished. The cursor is the server's clock, kept on disk; a pull
            // that fails leaves it where it was, and what did arrive is kept.
            val synced = try {
                device.sync.pull(connection)
            } catch (e: CancellationException) {
                throw e
            } catch (e: NotSignedIn) {
                throw e
            } catch (e: Exception) {
                device.sync.since()
            }
            shelf.syncedAt = synced
            _state.value = UiState.Library(first.items, synced, complete = first.complete)
            shelf.saveIfComplete()
            // Anything read offline since the last run goes now.
            device.outbox.flush(connection)
        } catch (e: CancellationException) {
            throw e
        } catch (e: NotSignedIn) {
            _state.value = UiState.SignedOut(server, e.message)
            return
        } catch (e: Exception) {
            val reason = e.message ?: "Could not read the library"
            val saved = device.shelf.load()
            _state.value = if (saved != null) {
                UiState.Library(saved.items, saved.syncedAt, offline = reason)
            } else {
                UiState.Unreachable(server, reason)
            }
            return
        }
        // The rest of the library, behind the first page. The saved shelf is
        // replaced only once all of it is here, so an offline launch never
        // shows fewer books than the last complete listing had.
        more(shelf, all = true)
    }

    /** One page more - or all of them - with the shelf on screen following along. */
    private suspend fun more(shelf: ShelfPager, all: Boolean = false) {
        suspend fun step(): Boolean {
            // A listing started again since - a retry, a new sign-in - has its own round.
            if (pager !== shelf) return false
            _state.update { if (it is UiState.Library && it.offline == null) it.copy(loadingMore = true) else it }
            val again = try {
                shelf.more()
            } catch (e: NotSignedIn) {
                _state.value = UiState.SignedOut(server, e.message)
                return false
            }
            val now = shelf.listing.value
            _state.update {
                if (it is UiState.Library && it.offline == null && pager === shelf) {
                    it.copy(items = now.items, complete = now.complete, loadingMore = false, failed = now.failed)
                } else {
                    it
                }
            }
            return again
        }
        if (all) {
            while (step()) Unit
        } else {
            step()
        }
    }

    private fun deviceName(): String =
        listOf(android.os.Build.MANUFACTURER, android.os.Build.MODEL)
            .filter { it.isNotBlank() }
            .joinToString(" ")
            .ifBlank { "Android" }

    override fun onCleared() {
        client?.close()
        super.onCleared()
    }

    companion object {
        private const val LAST_SEND_MS = 15_000L

        val Factory: ViewModelProvider.Factory = viewModelFactory {
            initializer {
                LibraryViewModel(checkNotNull(this[ViewModelProvider.AndroidViewModelFactory.APPLICATION_KEY]))
            }
        }
    }
}
