// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.api

import io.ktor.client.engine.mock.MockEngine
import io.ktor.client.engine.mock.respond
import io.ktor.client.engine.mock.toByteArray
import io.ktor.client.request.HttpRequestData
import io.ktor.http.HttpHeaders
import io.ktor.http.HttpStatusCode
import io.ktor.http.headersOf
import io.ktor.utils.io.ByteReadChannel
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test

/**
 * The client against a server that is not there.
 *
 * MockEngine rather than a running Lexarii, so these run on a JVM in
 * milliseconds and say exactly what went over the wire. What they cannot say
 * is whether the real server agrees - that is what the generated types are
 * for, and what a run against a live server checks.
 */
class LexariiClientTest {

    private val json = "application/json"

    private fun client(
        tokens: TokenStore = InMemoryTokenStore(),
        handler: (HttpRequestData) -> Pair<HttpStatusCode, String>,
    ): Pair<LexariiClient, MutableList<HttpRequestData>> {
        val seen = mutableListOf<HttpRequestData>()
        val engine = MockEngine { request ->
            seen += request
            val (status, body) = handler(request)
            respond(
                content = ByteReadChannel(body),
                status = status,
                headers = headersOf(HttpHeaders.ContentType, json),
            )
        }
        return LexariiClient("http://books.local:8080", tokens, engine) to seen
    }

    /**
     * The token endpoint, not the login: a login sets an HttpOnly cookie, which
     * an app cannot read and cannot send.
     */
    @Test
    fun `signing in asks for a token and keeps it`() = runTest {
        val tokens = InMemoryTokenStore()
        val (client, seen) = client(tokens) {
            HttpStatusCode.OK to """
                {"token":"abc123","sessionId":"s1","expiresAt":"2026-10-24T00:00:00Z",
                 "user":{"id":"u1","username":"adam","isAdmin":true,"createdAt":"2026-01-01T00:00:00Z"}}
            """.trimIndent()
        }

        val issued = client.signIn("adam", "hunter2", "Pixel 8")

        assertEquals("abc123", issued.token)
        assertEquals("adam", issued.user.username)
        assertEquals("/api/v1/auth/token", seen.single().url.encodedPath)
        // Held, so the next call carries it and a restart does not sign out.
        assertEquals("abc123", tokens.token())
        // With the server that issued it: a restart has to know where to go.
        assertEquals("http://books.local:8080", tokens.server())
    }

    /** The device name is required by the server, and is what makes a token revocable later. */
    @Test
    fun `the device name is sent`() = runTest {
        val (client, seen) = client {
            HttpStatusCode.OK to """
                {"token":"t","sessionId":"s","expiresAt":"2026-10-24T00:00:00Z",
                 "user":{"id":"u","username":"adam","isAdmin":false,"createdAt":"2026-01-01T00:00:00Z"}}
            """.trimIndent()
        }

        client.signIn("adam", "hunter2", "Adam's phone")

        val body = seen.single().body.toByteArray().decodeToString()
        assertTrue("device name missing from $body", body.contains("Adam's phone"))
    }

    /** Every authenticated call carries the bearer token. Without this the app is signed out on every request. */
    @Test
    fun `the token is sent as a bearer on later calls`() = runTest {
        val (client, seen) = client(InMemoryTokenStore("held-token")) {
            HttpStatusCode.OK to """{"items":[]}"""
        }

        client.library()

        assertEquals("Bearer held-token", seen.single().headers[HttpHeaders.Authorization])
    }

    /**
     * A 401 means the token is dead. It has to be thrown away, or the app
     * retries forever with a credential the server has already refused.
     */
    @Test
    fun `a refused token is discarded so the app can sign in again`() = runTest {
        val tokens = InMemoryTokenStore("stale", "http://books.local:8080")
        val (client, _) = client(tokens) {
            HttpStatusCode.Unauthorized to """{"error":{"code":"not_signed_in","message":"sign in to continue"}}"""
        }

        try {
            client.me()
            fail("a 401 did not raise NotSignedIn")
        } catch (e: NotSignedIn) {
            assertEquals("sign in to continue", e.message)
        }
        assertNull("the dead token was kept", tokens.token())
        // The address survives, so the sign-in form is already filled in.
        assertEquals("http://books.local:8080", tokens.server())
    }

    /**
     * The reader's WebView asks for images, and a chapter is untrusted input.
     * Only this book file's own assets may be fetched with the token.
     */
    @Test
    fun `an asset request cannot reach outside the one book file`() = runTest {
        val tokens = InMemoryTokenStore("abc123")
        val (client, seen) = client(tokens) { HttpStatusCode.OK to "" }
        val id = "f1"
        val refused = listOf(
            "/api/v1/auth/sessions",
            "/api/v1/books/f2/assets/cover.jpg",
            "/api/v1/books/f1/assets/../../f2/assets/x.jpg",
            "/api/v1/books/f1/assets/%2e%2e/%2E%2E/f2/file",
            "/api/v1/books/f1/assets/images/x.jpg?then=elsewhere",
        )
        for (path in refused) {
            try {
                client.asset(id, path)
                fail("fetched $path")
            } catch (e: IllegalArgumentException) {
                // refused before any request
            }
        }
        assertTrue("a refused path reached the network", seen.isEmpty())

        val asset = client.asset(id, "/api/v1/books/f1/assets/OEBPS/images/fig%201.png")
        assertEquals("Bearer abc123", seen.single().headers[HttpHeaders.Authorization])
        assertEquals("/api/v1/books/f1/assets/OEBPS/images/fig%201.png", seen.single().url.encodedPath)
        assertEquals(json, asset.contentType)
    }

    /**
     * A wrong password is a 401 too, and it has to say so. "Sign in to
     * continue" on the sign-in screen tells somebody nothing about what they
     * typed wrong.
     */
    @Test
    fun `a wrong password carries the server's reason`() = runTest {
        val (client, _) = client {
            HttpStatusCode.Unauthorized to
                """{"error":{"code":"invalid_credentials","message":"that username and password do not match"}}"""
        }

        try {
            client.signIn("nobody", "wrong", "Pixel 8")
            fail("a 401 did not raise NotSignedIn")
        } catch (e: NotSignedIn) {
            assertEquals("that username and password do not match", e.message)
        }
    }

    /**
     * The server has one error shape everywhere. Carrying its message through
     * is the difference between a screen that says what happened and one that
     * says "request failed".
     */
    @Test
    fun `a refusal carries the server's own reason`() = runTest {
        val (client, _) = client(InMemoryTokenStore("t")) {
            HttpStatusCode.Forbidden to
                """{"error":{"code":"not_an_admin","message":"only an administrator can change the shared library"}}"""
        }

        try {
            client.library()
            fail("a 403 was not raised")
        } catch (e: ServerRefused) {
            assertEquals(403, e.status)
            assertEquals("only an administrator can change the shared library", e.message)
        }
    }

    /** An older app must keep working when the server grows a field. */
    @Test
    fun `unknown fields do not break decoding`() = runTest {
        val (client, _) = client(InMemoryTokenStore("t")) {
            HttpStatusCode.OK to """
                {"items":[{"workId":"w1","title":"Dune","authors":["Frank Herbert"],
                 "formats":["epub"],"status":"ready","missing":false,"bookFileId":"b1",
                 "addedAt":"2026-01-01T00:00:00Z","canDownload":true,
                 "somethingAddedNextYear":"ignored"}]}
            """.trimIndent()
        }

        val page = client.library()

        assertEquals(1, page.items.size)
        assertEquals("Dune", page.items.single().title)
        assertEquals(listOf("Frank Herbert"), page.items.single().authors)
    }

    /** Optional fields are genuinely optional: a response that omits them decodes. */
    @Test
    fun `omitted optional fields decode as null`() = runTest {
        val (client, _) = client(InMemoryTokenStore("t")) {
            HttpStatusCode.OK to """
                {"items":[{"workId":"w1","title":"Dune","authors":[],"formats":["epub"],
                 "status":"ready","missing":false,"bookFileId":"b1",
                 "addedAt":"2026-01-01T00:00:00Z","canDownload":true}]}
            """.trimIndent()
        }

        val item = client.library().items.single()

        assertNull(item.series)
        assertNull(item.coverUrl)
        assertNull(item.progress)
    }

    /**
     * The sync cursor is the SERVER's clock, sent back verbatim. Substituting
     * the device's own would silently lose or repeat whatever the two clocks
     * differ by.
     */
    @Test
    fun `the sync cursor is sent back as the server gave it`() = runTest {
        val (client, seen) = client(InMemoryTokenStore("t")) {
            HttpStatusCode.OK to
                """{"until":"2026-09-24T08:00:00Z","more":false,"annotations":[],"deleted":[],"positions":[]}"""
        }

        val first = client.pull()
        client.pull(since = first.until)

        assertEquals("2026-09-24T08:00:00Z", first.until)
        assertNull("the first pull should send no cursor", seen[0].url.parameters["since"])
        assertEquals("2026-09-24T08:00:00Z", seen[1].url.parameters["since"])
    }

    /** Paging is by cursor, and the cursor is opaque. */
    @Test
    fun `library paging passes limit and cursor through`() = runTest {
        val (client, seen) = client(InMemoryTokenStore("t")) {
            HttpStatusCode.OK to """{"items":[],"nextCursor":"next-page"}"""
        }

        val page = client.library(limit = 50)
        client.library(limit = 50, cursor = page.nextCursor)

        assertEquals("50", seen[0].url.parameters["limit"])
        assertNull(seen[0].url.parameters["cursor"])
        assertEquals("next-page", seen[1].url.parameters["cursor"])
    }

    /** A server address with a trailing slash is what somebody types. */
    @Test
    fun `a trailing slash on the server address does not double up`() = runTest {
        val seen = mutableListOf<HttpRequestData>()
        val engine = MockEngine { request ->
            seen += request
            respond(ByteReadChannel("""{"items":[]}"""), HttpStatusCode.OK,
                headersOf(HttpHeaders.ContentType, json))
        }
        val client = LexariiClient("http://books.local:8080/", InMemoryTokenStore("t"), engine)

        client.library()

        assertEquals("/api/v1/library", seen.single().url.encodedPath)
    }
}
