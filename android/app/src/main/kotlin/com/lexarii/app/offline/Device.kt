// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.offline

import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.File

/**
 * Everything this app keeps for the signed-in account, under one directory
 * in its private storage, so that one account's things can be removed whole.
 *
 * `owner` names the account - server and user id - that the rest belongs to.
 *
 * PDF pages being read from the server go in `cacheDir` instead, where the
 * system may reclaim them; they are this account's too, and are cleared with
 * the rest.
 */
class Device(
    filesDir: File,
    private val io: CoroutineDispatcher = Dispatchers.IO,
    cacheDir: File? = null,
) {
    private val root = File(filesDir, "account")
    private val owner = File(root, "owner")

    val kept = OfflineStore(File(root, "kept"), io)
    val outbox = Outbox(File(root, "outbox.json"), io)
    val shelf = ShelfCache(File(root, "shelf.json"), io)
    val marks = MarkStore(File(root, "marks"), io)
    val sync = Sync(File(root, "sync.json"), marks, outbox, io)
    val covers = CoverStore(File(root, "covers"), io)
    val pages = PageCache(cacheDir?.let { File(it, "account-pages") } ?: File(root, "pages"), io)

    /** Who the things here belong to, or null if nobody has claimed them. */
    suspend fun owner(): String? = withContext(io) { if (owner.isFile) owner.readText() else null }

    /**
     * Records who is signed in. If something here belonged to a different
     * account, it is removed first, and this returns true.
     */
    suspend fun claim(account: String): Boolean {
        val (previous, anything) = withContext(io) {
            (if (owner.isFile) owner.readText() else null) to !root.list().isNullOrEmpty()
        }
        // Things here with no owner named are nobody's this account can vouch
        // for - left by work that outlived a sign-out, or by a sign-out killed
        // halfway - so they are treated as somebody else's.
        val someoneElse = if (previous != null) previous != account else anything
        if (someoneElse) clear()
        withContext(io) { owner.writeAtomically(account.toByteArray()) }
        outbox.reopen()
        marks.reopen()
        sync.reopen()
        shelf.reopen()
        covers.reopen()
        pages.reopen()
        return someoneElse
    }

    /**
     * Removes everything, and closes the stores to writes until the next
     * claim: whatever the last account still had in flight lands nowhere.
     */
    suspend fun clear() {
        kept.clear()
        outbox.clear()
        marks.clear()
        sync.clear()
        shelf.clear()
        covers.clear()
        pages.clear()
        withContext(io) { root.deleteRecursively() }
    }
}
