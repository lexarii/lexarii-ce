// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.api

import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The generated types against what the server actually sends.
 *
 * The fixtures in src/test/resources were captured from a running Lexarii
 * with two real books in it - not written by hand. Hand-written JSON is how a
 * test comes to assert a shape the server never produces: the first version of
 * the sync fixture in LexariiClientTest omitted two fields the server always
 * sends, and it was the generated types that noticed, not the author.
 *
 * This is the claim the whole generator exists to support - that a client built
 * from the Go can read what the Go serves - so it is checked rather than
 * assumed. Recapture these with a running server if the wire types change in a
 * way that makes them stale.
 */
class RealResponsesTest {

    private val json = Json { ignoreUnknownKeys = true; explicitNulls = false }

    private fun fixture(name: String): String =
        checkNotNull(javaClass.classLoader?.getResourceAsStream(name)) { "missing fixture $name" }
            .bufferedReader().readText()

    @Test
    fun `a real library page decodes`() {
        val page = json.decodeFromString<LibraryPage>(fixture("library.json"))

        assertEquals(2, page.items.size)
        val algebra = page.items.first { it.title == "Algebra Essentials" }
        assertEquals(listOf("David A. Santos", "Olgha B. Davis"), algebra.authors)
        assertEquals(listOf("epub"), algebra.formats)
        assertEquals("ready", algebra.status)
        assertFalse(algebra.missing)
        // Present on this book, and the generated type says it may be absent.
        assertEquals(27, algebra.unitCount)
        assertEquals("section", algebra.unitType)
        assertNotNull(algebra.coverUrl)
        // Never read, so no progress - which must decode as absent rather than
        // as zero. "Not started" and "at the beginning" are different things.
        assertEquals(null, algebra.progress)
    }

    @Test
    fun `a real sync pull decodes`() {
        val pull = json.decodeFromString<SyncPull>(fixture("sync.json"))

        assertFalse(pull.more)
        assertTrue(pull.annotations.isEmpty())
        assertTrue(pull.deleted.isEmpty())
        assertTrue(pull.positions.isEmpty())
        // The cursor for the next pull. The server's own clock, with an offset
        // on it, and it has to survive the round trip as text.
        assertTrue("until was ${pull.until}", pull.until.startsWith("2026-"))
    }

    @Test
    fun `a real user decodes`() {
        val user = json.decodeFromString<User>(fixture("me.json"))

        assertEquals("adam", user.username)
        assertTrue("the first account should be an administrator", user.isAdmin)
    }
}
