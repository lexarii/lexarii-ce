// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app

import com.lexarii.app.api.InMemoryTokenStore
import com.lexarii.app.api.TokenStore
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.CoroutineStart
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch

/**
 * The order between signing in and signing out, which share one device, one
 * token store, and one directory of account data.
 *
 * A sign-out sends what it can and then clears the token and the device; a
 * sign-in takes both over. Overlapping, the sign-out's clear lands on the new
 * account - its token gone, its storage emptied - or its last send goes out
 * with the new account's token, filing the previous person's notes as the
 * new person's own. So every sign-in and sign-out takes a turn, and each gets
 * a check that says whether the turn is still its own before it does anything
 * that cannot be undone.
 *
 * Main-thread only, like the ViewModel that holds it.
 */
class AccountTurns {
    private var generation = 0
    private var leaving: Job? = null

    /** Whether a sign-out is running. */
    val signingOut: Boolean get() = leaving?.isActive == true

    /**
     * Starts a sign-out, or returns null if one is already running. `work` is
     * handed a check that stays true until a sign-in begins; clearing the
     * token and the device must ask it first.
     */
    fun leave(scope: CoroutineScope, work: suspend (stillOurs: () -> Boolean) -> Unit): Job? {
        if (signingOut) return null
        val mine = ++generation
        // Held before it runs: on the main thread's immediate dispatcher a
        // launch runs until its first suspension straight away, and a sign-in
        // looking for a sign-out to wait on must already be able to see it.
        val job = scope.launch(start = CoroutineStart.LAZY) { work { generation == mine } }
        leaving = job
        job.start()
        return job
    }

    /**
     * The very start of a sign-in, before anything is asked of a server:
     * waits for a sign-out still finishing, then takes the turn. The check it
     * returns turns false the moment a sign-out, or a later sign-in, begins -
     * and a sign-in overtaken like that must not save its token or claim the
     * device.
     */
    suspend fun enter(): () -> Boolean {
        leaving?.join()
        val mine = ++generation
        return { generation == mine }
    }
}

/**
 * The token as it is now, in a store of its own.
 *
 * A signing-out account's last send reads its token from this: a sign-in
 * that saves a new token to the shared store cannot lend it to the previous
 * account's notes, and a 401 on the way out clears only this copy.
 */
suspend fun TokenStore.frozen(): TokenStore = InMemoryTokenStore(token(), server())
