// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app

import com.lexarii.app.api.InMemoryTokenStore
import com.lexarii.app.api.TokenStore
import com.lexarii.app.offline.Device
import com.lexarii.app.testing.FakeServer
import com.lexarii.app.testing.device
import io.ktor.http.HttpStatusCode
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

/**
 * One run of keeping a BookFile, as the background worker runs it: for whom,
 * with which token, and whether a failure is tried again.
 */
class KeepJobTest {
    @get:Rule
    val tmp = TemporaryFolder()

    private val account = "http://books.local:8080 user-a"

    /** What the notification was told, in order. */
    private class Recorded : KeepProgress {
        val events = mutableListOf<String>()
        var allowForeground = true
        override suspend fun foreground(): Boolean {
            events += "foreground"
            return allowForeground
        }
        override fun update(done: Long, total: Long, inBytes: Boolean) {
            events += "update"
        }
        override fun finish() {
            events += "finish"
        }
    }

    private suspend fun TestScope.setUp(server: FakeServer, tokens: TokenStore = InMemoryTokenStore("t", "http://books.local:8080")): Pair<Device, KeepJob> {
        val device = device(tmp.root)
        device.claim(account)
        val job = KeepJob(device, tokens, clientFor = { _, own -> server.client(this, own) }, maxAttempts = 3)
        return device to job
    }

    @Test
    fun `a book is kept, and the notification goes when it is`() = runTest {
        val server = FakeServer()
        val (device, job) = setUp(server)
        val progress = Recorded()
        assertEquals(KeepJob.Outcome.Kept, job.run(server.bookFileId, account, attempt = 0, progress))
        assertTrue(device.kept.has(server.bookFileId))
        assertEquals("foreground", progress.events.first())
        assertEquals("finish", progress.events.last())
    }

    /**
     * Refused a foreground service - started in the background, when Wi-Fi
     * came - the run posts its own notification; it must still be taken away,
     * however the run ends.
     */
    @Test
    fun `a run that could not be a foreground service still takes its notification away`() = runTest {
        val server = FakeServer()
        server.broken += "/api/v1/books/${server.bookFileId}/spine/1"
        val (_, job) = setUp(server)
        val progress = Recorded().apply { allowForeground = false }
        assertEquals(KeepJob.Outcome.Retry, job.run(server.bookFileId, account, attempt = 0, progress))
        assertEquals("finish", progress.events.last())
        assertEquals(1, progress.events.count { it == "finish" })
    }

    /** Asked for by somebody who has since signed out, or been replaced. */
    @Test
    fun `nothing is fetched for an account that is no longer signed in here`() = runTest {
        val server = FakeServer()
        val (device, job) = setUp(server)
        device.claim("http://books.local:8080 user-b")
        val progress = Recorded()
        val outcome = job.run(server.bookFileId, account, attempt = 0, progress)
        assertTrue(outcome is KeepJob.Outcome.Failed)
        assertTrue("asked the server for something", server.seen.isEmpty())
        assertEquals(listOf("finish"), progress.events)
    }

    @Test
    fun `no token is a failure, not a retry`() = runTest {
        val server = FakeServer()
        val (_, job) = setUp(server, tokens = InMemoryTokenStore(null, "http://books.local:8080"))
        assertEquals(KeepJob.Outcome.Failed("Sign in again to keep this book"), job.run(server.bookFileId, account, 0, Recorded()))
        assertTrue(server.seen.isEmpty())
    }

    /**
     * The token is copied when the run starts. A 401 clears that copy, never
     * the app's - and whatever the app's store holds later, this run never
     * sends it.
     */
    @Test
    fun `a lapsed token fails the run, and leaves the app's own token alone`() = runTest {
        val server = FakeServer()
        server.refused["/api/v1/books/${server.bookFileId}/manifest"] = HttpStatusCode.Unauthorized
        val app = InMemoryTokenStore("t", "http://books.local:8080")
        val (_, job) = setUp(server, tokens = app)
        assertEquals(KeepJob.Outcome.Failed("Sign in again to keep this book"), job.run(server.bookFileId, account, 0, Recorded()))
        assertEquals("t", app.token())
    }

    @Test
    fun `the token a run sends is the one it started with`() = runTest {
        val server = FakeServer()
        val app = InMemoryTokenStore("token-a", "http://books.local:8080")
        val (_, job) = setUp(server, tokens = app)
        val progress = object : KeepProgress {
            override suspend fun foreground(): Boolean {
                // The next person's sign-in, landing mid-run.
                app.save("token-b", "http://books.local:8080")
                return true
            }
            override fun update(done: Long, total: Long, inBytes: Boolean) = Unit
            override fun finish() = Unit
        }
        job.run(server.bookFileId, account, 0, progress)
        val sent = server.seen.map { it.headers["Authorization"] }.toSet()
        assertEquals(setOf("Bearer token-a"), sent)
    }

    @Test
    fun `a refusal that will not change fails, and a fault of the server's is tried again`() = runTest {
        val server = FakeServer()
        val (_, job) = setUp(server)

        server.refused["/api/v1/books/${server.bookFileId}/manifest"] = HttpStatusCode.Forbidden
        val refused = job.run(server.bookFileId, account, 0, Recorded())
        assertEquals(KeepJob.Outcome.Failed("refused with 403"), refused)

        server.refused["/api/v1/books/${server.bookFileId}/manifest"] = HttpStatusCode.TooManyRequests
        assertEquals(KeepJob.Outcome.Retry, job.run(server.bookFileId, account, 0, Recorded()))

        server.refused.clear()
        server.broken += "/api/v1/books/${server.bookFileId}/manifest"
        assertEquals(KeepJob.Outcome.Retry, job.run(server.bookFileId, account, 0, Recorded()))
        // Only so many times: then it says so, and Keep starts it again.
        assertEquals(KeepJob.Outcome.Failed("boom"), job.run(server.bookFileId, account, 3, Recorded()))

        server.broken.clear()
        server.offline = true
        assertEquals(KeepJob.Outcome.Retry, job.run(server.bookFileId, account, 1, Recorded()))
    }

    @Test
    fun `a format that cannot be kept fails at once`() = runTest {
        val server = FakeServer()
        server.manifest = """{"bookFileId":"${server.bookFileId}","format":"cbz","spine":[],"toc":[]}"""
        val (device, job) = setUp(server)
        val outcome = job.run(server.bookFileId, account, 0, Recorded())
        assertTrue(outcome is KeepJob.Outcome.Failed)
        assertFalse(device.kept.has(server.bookFileId))
        assertNull(device.kept.manifest(server.bookFileId))
    }
}
