// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app

import com.lexarii.app.api.InMemoryTokenStore
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Signing in and signing out, overlapping: neither may undo the other, and the
 * previous person's last notes must never go out under the next person's
 * token.
 */
class AccountTurnsTest {
    @Test
    fun `a sign-in waits for a sign-out still finishing, which then clears as it should`() = runTest {
        val turns = AccountTurns()
        val sending = CompletableDeferred<Unit>()
        var cleared: Boolean? = null
        turns.leave(this) { stillOurs ->
            sending.await()
            cleared = stillOurs()
        }
        advanceUntilIdle()

        var entered: (() -> Boolean)? = null
        val signingIn = launch { entered = turns.enter() }
        advanceUntilIdle()
        assertNull("the sign-in must not begin while the sign-out runs", entered)

        sending.complete(Unit)
        signingIn.join()
        assertEquals("nothing overtook the sign-out, so it clears", true, cleared)
        assertTrue("and then the sign-in has the device", entered!!())
    }

    /** A sign-in already asking the server when somebody signs out must not save its token afterwards. */
    @Test
    fun `a sign-out overtakes a sign-in already in flight`() = runTest {
        val turns = AccountTurns()
        val ours = turns.enter()
        var stillOursAtClear: Boolean? = null
        turns.leave(this) { stillOurs -> stillOursAtClear = stillOurs() }
        advanceUntilIdle()

        assertFalse("the overtaken sign-in must not claim the device", ours())
        assertEquals(true, stillOursAtClear)
    }

    @Test
    fun `only one sign-out runs at a time`() = runTest {
        val turns = AccountTurns()
        val sending = CompletableDeferred<Unit>()
        var runs = 0
        turns.leave(this) { runs++; sending.await() }
        advanceUntilIdle()
        assertTrue(turns.signingOut)
        assertNull(turns.leave(this) { runs++ })
        sending.complete(Unit)
        advanceUntilIdle()
        assertEquals(1, runs)
    }

    /** The last send of a signing-out account goes with its own token, whatever is saved after. */
    @Test
    fun `a frozen token is not the next one saved, and clearing it clears nothing shared`() = runTest {
        val shared = InMemoryTokenStore("previous-person", "http://books.local:8080")
        val own = shared.frozen()

        shared.save("next-person", "http://books.local:8080")
        assertEquals("previous-person", own.token())

        own.clear()
        assertEquals("next-person", shared.token())
    }
}
