// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app

import com.lexarii.app.api.InMemoryTokenStore
import com.lexarii.app.api.NotSignedIn
import com.lexarii.app.api.ServerRefused
import com.lexarii.app.api.LexariiClient
import com.lexarii.app.api.TokenStore
import com.lexarii.app.offline.Device
import kotlinx.coroutines.CancellationException

/**
 * What a download run shows while it runs, and takes away when it ends.
 * An interface so the job can be tested without a notification manager.
 */
interface KeepProgress {
    /** Asks to run as a foreground service. False if Android would not allow one now. */
    suspend fun foreground(): Boolean

    fun update(done: Long, total: Long, inBytes: Boolean)

    /**
     * The run is over, however it ended. Whatever this run showed goes, and
     * nothing is shown again after it.
     */
    fun finish()
}

/**
 * One run of keeping one BookFile, apart from WorkManager: who it is for,
 * what it sends, and whether a failure is worth trying again.
 *
 * `KeepWorker` is only the adapter around this.
 */
class KeepJob(
    private val device: Device,
    private val tokens: TokenStore,
    private val clientFor: (server: String, tokens: TokenStore) -> LexariiClient,
    private val maxAttempts: Int = MAX_ATTEMPTS,
    private val clock: () -> Long = System::currentTimeMillis,
) {
    sealed interface Outcome {
        data object Kept : Outcome
        data object Retry : Outcome
        data class Failed(val message: String) : Outcome
    }

    /**
     * Keeps `bookFileId` for `account`; `attempt` counts runs before this one.
     *
     * `progress` is finished whatever happens, so a run Android would not
     * make a foreground service - one started in the background, when Wi-Fi
     * arrives or a retry comes due - never leaves its notification behind.
     */
    suspend fun run(bookFileId: String, account: String, attempt: Int, progress: KeepProgress): Outcome {
        try {
            // The token as it is now, in a store of its own, read BEFORE the
            // owner is checked: a sign-in landing between the two changes the
            // owner first and the token after, so a token read here that is
            // somebody else's goes with an owner check that fails. Nothing
            // this run sends can then carry the next person's token, and a
            // 401 clears this copy, not the app's.
            val own = InMemoryTokenStore(tokens.token(), tokens.server())
            // Asked for by somebody who has since signed out, or been replaced
            // by somebody else: their book is not fetched into this account.
            if (device.owner() != account) return Outcome.Failed("Signed out before this book was kept")
            val server = own.server()
            if (server == null || own.token() == null) return Outcome.Failed("Sign in again to keep this book")

            progress.foreground()
            val client = clientFor(server, own)
            var reported = 0L
            return try {
                device.kept.download(client, bookFileId) { done, total, bytes ->
                    val now = clock()
                    // Often enough to move a bar, rarely enough not to flood
                    // the system with a message per 64 KB.
                    if (now - reported < PROGRESS_MS && done != total) return@download
                    reported = now
                    progress.update(done, total, bytes)
                }
                Outcome.Kept
            } catch (e: CancellationException) {
                throw e
            } catch (e: NotSignedIn) {
                Outcome.Failed("Sign in again to keep this book")
            } catch (e: ServerRefused) {
                // The server said no and will say it again: a book no longer
                // shared, a download not permitted. Its own fault, or
                // overloaded, is worth another try.
                if (e.status in 400..499 && e.status != 408 && e.status != 429) {
                    Outcome.Failed(e.message ?: "The server refused")
                } else {
                    retry(e, attempt)
                }
            } catch (e: IllegalArgumentException) {
                Outcome.Failed(e.message ?: "This book cannot be kept")
            } catch (e: Exception) {
                retry(e, attempt)
            } finally {
                client.close()
            }
        } finally {
            progress.finish()
        }
    }

    private fun retry(e: Exception, attempt: Int): Outcome =
        if (attempt >= maxAttempts) Outcome.Failed(e.message ?: "The download failed") else Outcome.Retry

    companion object {
        private const val PROGRESS_MS = 500L

        // Enough to cover a night of patchy signal at the backoff's doubling
        // intervals; after that it says it failed, and Keep starts it again.
        const val MAX_ATTEMPTS = 12
    }
}
