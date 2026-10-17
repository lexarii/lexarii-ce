// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.api

import io.ktor.client.HttpClient
import io.ktor.client.HttpClientConfig
import io.ktor.client.call.body
import io.ktor.client.engine.HttpClientEngine
import io.ktor.client.plugins.contentnegotiation.ContentNegotiation
import io.ktor.client.request.get
import io.ktor.client.request.header
import io.ktor.client.request.parameter
import io.ktor.client.request.post
import io.ktor.client.request.prepareGet
import io.ktor.client.request.setBody
import io.ktor.client.statement.HttpResponse
import io.ktor.client.statement.bodyAsChannel
import io.ktor.utils.io.readAvailable
import io.ktor.http.ContentType
import io.ktor.http.HttpStatusCode
import io.ktor.http.contentType
import io.ktor.serialization.kotlinx.json.json
import kotlinx.serialization.json.Json

/**
 * Everything this client knows how to ask the server.
 *
 * The wire types are generated from the Go in `Types.gen.kt`, so this file
 * holds only the calls. A renamed json tag is a compile error here rather than
 * a surprise on somebody's phone.
 */
class LexariiClient(
    private val baseUrl: String,
    private val tokens: TokenStore,
    engine: HttpClientEngine? = null,
) {
    private val http: HttpClient = build(engine)

    /**
     * Sign in and keep the token.
     *
     * `/auth/token` rather than `/auth/login`: the login sets an HttpOnly
     * cookie, which is right for a browser and useless to an app. The device
     * name is required by the server and is what makes this token revocable a
     * year from now from a list on another machine.
     */
    suspend fun signIn(username: String, password: String, device: String): AuthToken {
        val issued: AuthToken = http.post(url("/api/v1/auth/token")) {
            contentType(ContentType.Application.Json)
            setBody(TokenRequest(username = username, password = password, device = device))
        }.orThrow().body()

        tokens.save(issued.token, baseUrl)
        return issued
    }

    /** Who the held token belongs to, used to check it is still good. */
    suspend fun me(): User = http.get(url("/api/v1/auth/me")) { authorise() }.orThrow().body()

    /** One page of the library. */
    suspend fun library(limit: Int? = null, cursor: String? = null): LibraryPage =
        http.get(url("/api/v1/library")) {
            authorise()
            limit?.let { parameter("limit", it) }
            cursor?.let { parameter("cursor", it) }
        }.orThrow().body()

    /**
     * Everything that changed since a moment, and anything made offline.
     *
     * The two halves of the sync protocol. `since` is the server's own clock
     * from the previous pull, never this device's: two clocks a minute apart
     * would silently lose or repeat a minute of somebody's reading.
     */
    suspend fun pull(since: String? = null, cursor: String? = null, limit: Int? = null): SyncPull =
        http.get(url("/api/v1/sync")) {
            authorise()
            // A cursor carries on a round in progress, and the server ignores
            // `since` beside one; sending only the cursor says so plainly.
            if (cursor != null) parameter("cursor", cursor) else since?.let { parameter("since", it) }
            limit?.let { parameter("limit", it) }
        }.orThrow().body()

    suspend fun push(body: SyncPush): SyncPushResult =
        http.post(url("/api/v1/sync")) {
            authorise()
            contentType(ContentType.Application.Json)
            setBody(body)
        }.orThrow().body()

    /** The spine and contents of an EPUB, or the contents alone of a PDF. */
    suspend fun manifest(bookFileId: String): ReaderManifest =
        http.get(url("/api/v1/books/${segment(bookFileId)}/manifest")) { authorise() }.orThrow().body()

    /** One chapter, sanitised on the server, with its image paths rewritten to `assets()`. */
    suspend fun spine(bookFileId: String, index: Int): SpineContent =
        http.get(url("/api/v1/books/${segment(bookFileId)}/spine/$index")) { authorise() }.orThrow().body()

    /**
     * Every mark this person can see in one BookFile: their own, and those
     * somebody has shared with them. The sync pull carries only the first;
     * this is where the second come from.
     */
    suspend fun annotations(bookFileId: String): AnnotationList =
        http.get(url("/api/v1/books/${segment(bookFileId)}/annotations")) { authorise() }.orThrow().body()

    /** Where this person left off, merged across every device they read on. */
    suspend fun progress(bookFileId: String): ReadingProgress =
        http.get(url("/api/v1/books/${segment(bookFileId)}/progress")) { authorise() }.orThrow().body()

    /**
     * One image from inside a book, as its type and its bytes.
     *
     * Only the asset path of the one book is accepted, not an arbitrary URL:
     * this is what the reader's WebView asks for, and a chapter is untrusted
     * input. A crafted image path must not be able to make the app spend its
     * bearer token anywhere else on the server.
     */
    suspend fun asset(bookFileId: String, path: String): Asset {
        val prefix = "/api/v1/books/${segment(bookFileId)}/assets/"
        // Decoded before the dot check: OkHttp resolves `%2e%2e` as `..`.
        val climbs = path.split('/').any {
            runCatching { java.net.URLDecoder.decode(it, "UTF-8") }.getOrDefault("..") in setOf(".", "..")
        }
        require(path.startsWith(prefix) && !climbs && '?' !in path && '#' !in path) {
            "not an asset of this book file: $path"
        }
        val response = http.get(url(path)) { authorise() }.orThrow()
        return Asset(response.headers["Content-Type"] ?: "application/octet-stream", response.body())
    }

    /**
     * What a BookFile is before it is opened: its format, how many pages a
     * PDF has, and whether this person may take the file itself.
     */
    suspend fun info(bookFileId: String): BookInfo =
        http.get(url("/api/v1/books/${segment(bookFileId)}/info")) { authorise() }.orThrow().body()

    /**
     * A BookFile's cover, as its type and bytes.
     *
     * Built from the id rather than taken from `coverUrl`, so a value in a
     * listing can only ever say whether there is a cover, never where the app
     * sends its token. Bounded: a cover is decoded on the phone, and a server
     * answering with something enormous must not take the app down with it.
     */
    suspend fun cover(bookFileId: String): Asset {
        val response = http.get(url("/api/v1/books/${segment(bookFileId)}/cover")) { authorise() }.orThrow()
        val bytes = response.readCapped(MAX_COVER_BYTES) ?: throw java.io.IOException("the cover is too large to show")
        return Asset(response.headers["Content-Type"] ?: "application/octet-stream", bytes)
    }

    /**
     * One page of a PDF, as a one-page PDF document.
     *
     * `prefetch` says the page was fetched ahead of the reader rather than
     * looked at, which the server keeps out of the record of what a borrowed
     * book's reader actually read.
     */
    suspend fun pdfPage(bookFileId: String, page: Int, prefetch: Boolean = false): ByteArray {
        val response = http.get(url("/api/v1/books/${segment(bookFileId)}/page/$page")) {
            authorise()
            if (prefetch) parameter("prefetch", "1")
        }.orThrow()
        return response.readCapped(MAX_PAGE_BYTES) ?: throw java.io.IOException("page $page is too large to show")
    }

    /**
     * The original file, from byte `from` on, handed to `sink` a chunk at a
     * time as it arrives - never held whole, because a PDF can run to hundreds
     * of megabytes.
     *
     * `validator` is the Last-Modified of the bytes already held. The server
     * sends the rest only if the file has not changed since; if it has, it
     * sends the whole file again, and `Download.restarted` says so. Without
     * the check a resumed download could splice two versions of a book.
     */
    suspend fun download(
        bookFileId: String,
        from: Long,
        validator: String?,
        begin: suspend (Download) -> Unit,
        sink: suspend (ByteArray, Int) -> Unit,
    ) {
        http.prepareGet(url("/api/v1/books/${segment(bookFileId)}/file")) {
            authorise()
            if (from > 0) {
                header("Range", "bytes=$from-")
                validator?.let { header("If-Range", it) }
            }
        }.execute { response ->
            if (response.status == HttpStatusCode.RequestedRangeNotSatisfiable && from > 0) {
                // Asked to resume from the very end: everything is here already.
                begin(Download(restarted = false, total = from, validator = validator))
                return@execute
            }
            response.orThrow()
            val partial = response.status == HttpStatusCode.PartialContent
            val total = if (partial) {
                // "bytes <first>-<last>/<length>". A part that does not start
                // where it was asked to would be appended in the wrong place.
                val range = response.headers["Content-Range"]
                val first = range?.removePrefix("bytes ")?.substringBefore('-')?.trim()?.toLongOrNull()
                if (first != from) throw java.io.IOException("the server sent a part from byte $first, not $from")
                range?.substringAfterLast('/')?.toLongOrNull()
            } else {
                response.headers["Content-Length"]?.toLongOrNull()
            }
            begin(Download(restarted = !partial, total = total, validator = response.headers["Last-Modified"]))
            val channel = response.bodyAsChannel()
            val buffer = ByteArray(64 * 1024)
            while (true) {
                val read = channel.readAvailable(buffer, 0, buffer.size)
                if (read == -1) break
                if (read > 0) sink(buffer, read)
            }
        }
    }

    /**
     * Marks the reader could not place, and marks flagged earlier that it now
     * could. The server only ever flags or unflags; nothing is deleted.
     */
    suspend fun reportOrphans(bookFileId: String, orphaned: List<String>, resolved: List<String>): OrphanResult =
        http.post(url("/api/v1/books/${segment(bookFileId)}/annotations/orphaned")) {
            authorise()
            contentType(ContentType.Application.Json)
            setBody(OrphanAnnotations(ids = orphaned, resolved = resolved.takeIf { it.isNotEmpty() }))
        }.orThrow().body()

    fun close() = http.close()

    /** The body, or null if it is longer than `limit` bytes. */
    private suspend fun HttpResponse.readCapped(limit: Int): ByteArray? {
        val declared = headers["Content-Length"]?.toLongOrNull()
        if (declared != null && declared > limit) return null
        val channel = bodyAsChannel()
        val out = java.io.ByteArrayOutputStream()
        val buffer = ByteArray(16 * 1024)
        while (true) {
            val read = channel.readAvailable(buffer, 0, buffer.size)
            if (read == -1) break
            out.write(buffer, 0, read)
            if (out.size() > limit) return null
        }
        return out.toByteArray()
    }

    /** An id goes into a path, so it is escaped even though a UUID never needs it. */
    private fun segment(id: String) = java.net.URLEncoder.encode(id, "UTF-8")

    private fun url(path: String) = baseUrl.trimEnd('/') + path

    private suspend fun io.ktor.client.request.HttpRequestBuilder.authorise() {
        tokens.token()?.let { header("Authorization", "Bearer $it") }
    }

    /**
     * Turn a refusal into something with a reason in it.
     *
     * The server answers with one error shape everywhere, and throwing it
     * whole means a screen can say what actually happened instead of "request
     * failed". A 401 is separated out because it is the one the app must act
     * on rather than report: the token is gone and the person has to sign in.
     */
    private suspend fun HttpResponse.orThrow(): HttpResponse {
        if (status.isSuccess()) return this
        if (status == HttpStatusCode.Unauthorized) {
            tokens.clear()
            throw NotSignedIn(readError()?.message ?: "sign in to continue")
        }
        throw ServerRefused(status.value, readError()?.message ?: status.description)
    }

    private suspend fun HttpResponse.readError(): ErrorDetail? = runCatching { body<Error>().error }.getOrNull()

    private fun build(engine: HttpClientEngine?): HttpClient {
        val configure: HttpClientConfig<*>.() -> Unit = {
            install(ContentNegotiation) {
                json(
                    Json {
                        // The server adds fields without asking, and an older
                        // app must keep working when it does.
                        ignoreUnknownKeys = true
                        explicitNulls = false
                    }
                )
            }
            expectSuccess = false
        }
        return if (engine != null) HttpClient(engine, configure) else HttpClient(configure)
    }
}

/** An image from a book, for the reader to hand to its WebView. */
class Asset(val contentType: String, val bytes: ByteArray)

/**
 * How a file download began: whether it starts over from byte zero (the
 * server would not resume, or the file changed), how long the whole file is
 * if the server said, and the validator to resume it with next time.
 */
class Download(val restarted: Boolean, val total: Long?, val validator: String?)

// A cover is a thumbnail's source, and a page is one page: anything past
// these is not what it claims to be.
private const val MAX_COVER_BYTES = 8 * 1024 * 1024
private const val MAX_PAGE_BYTES = 64 * 1024 * 1024

/** The token is gone or was never good. The caller signs in again. */
class NotSignedIn(message: String) : Exception(message)

/** The server said no, and said why. */
class ServerRefused(val status: Int, message: String) : Exception(message)

private fun HttpStatusCode.isSuccess() = value in 200..299
