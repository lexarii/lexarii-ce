// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.testing

import com.lexarii.app.api.InMemoryTokenStore
import com.lexarii.app.api.PushedMark
import com.lexarii.app.api.LexariiClient
import com.lexarii.app.api.SyncPush
import com.lexarii.app.offline.Device
import io.ktor.client.engine.mock.MockEngine
import io.ktor.client.engine.mock.MockEngineConfig
import io.ktor.client.engine.mock.respond
import io.ktor.client.engine.mock.toByteArray
import io.ktor.client.request.HttpRequestData
import io.ktor.http.HttpHeaders
import io.ktor.http.HttpMethod
import io.ktor.http.HttpStatusCode
import io.ktor.http.headersOf
import io.ktor.utils.io.ByteReadChannel
import kotlinx.coroutines.test.StandardTestDispatcher
import kotlinx.coroutines.test.TestScope
import kotlinx.serialization.json.Json
import java.io.File

/**
 * A Lexarii that answers the reader's requests from memory, and can be
 * switched off to play at having no network.
 */
class FakeServer(val bookFileId: String = "6f1c7a52-0000-4000-8000-000000000001") {
    var manifest = """
        {"bookFileId":"$bookFileId","format":"epub",
         "spine":[{"index":0,"linear":true},{"index":1,"linear":true},{"index":2,"linear":true}],
         "toc":[]}
    """.trimIndent()
    var progress: Pair<HttpStatusCode, String> = HttpStatusCode.OK to """{"bookFileId":"$bookFileId","started":false}"""
    var offline = false

    /** Chapter HTML by spine index; anything missing is `<p>chapter N</p>`. */
    val chapters = mutableMapOf<Int, String>()

    /** Asset bytes by the path the server writes into a chapter. */
    val assets = mutableMapOf<String, ByteArray>()

    /** Paths that answer 500, to fail a download partway. */
    val broken = mutableSetOf<String>()

    /** Paths that answer with a refusal of their own: 401, 403, 404. */
    val refused = mutableMapOf<String, HttpStatusCode>()

    val seen = mutableListOf<HttpRequestData>()
    val pushed = mutableListOf<String>()

    /** Sync pull pages, answered in order; once they run out, an empty pull at `until`. */
    val pulls = ArrayDeque<Pair<HttpStatusCode, String>>()
    var until = "2026-09-24T20:00:00Z"

    /** The per-book list of marks, own and shared. */
    var annotationList = """{"items":[]}"""

    /** How each pushed mark is answered, by id. Anything not here is applied. */
    val markStatus = mutableMapOf<String, String>()

    /** How many pushes to take and then fail - the response lost on the way back. */
    var losePushResponses = 0

    /** Held open until completed: a sync pull, or a push, still in the air. */
    var pullGate: kotlinx.coroutines.CompletableDeferred<Unit>? = null
    var pushGate: kotlinx.coroutines.CompletableDeferred<Unit>? = null

    /** Held open until completed: a PDF page request still in the air. */
    var pageGate: kotlinx.coroutines.CompletableDeferred<Unit>? = null

    /**
     * Marks the sync pull pages through the way the server does: in order,
     * `limit` at a time, each page after the first found by the opaque cursor
     * the page before handed out - so rows sharing one timestamp page cleanly.
     * When set, it answers the pulls instead of `pulls`.
     */
    var syncRows: List<String>? = null

    /** A cursor the pull fails at, to cut a round off halfway. */
    var failPullAt: String? = null

    /** The library, as listing items, paged by `limit` and an `after-N` cursor. */
    var library: List<String> = emptyList()

    /** A library cursor that fails, to cut the listing off halfway. */
    var failLibraryAt: String? = null

    /** `/info` for the one BookFile. */
    var info = """{"bookFileId":"$bookFileId","title":"A Book","workId":"w1","format":"epub","sizeBytes":0,"status":"ready","reflowable":true,"canDownload":true}"""

    /** The original file `/file` serves, with range requests, as the server's ServeContent does. */
    var file = ByteArray(0)
    var lastModified = "Wed, 24 Sep 2026 20:00:00 GMT"

    /** Bytes to send of the next `/file` response before the connection "drops". */
    var cutFileAfter: Int? = null

    /** How far past the asked-for byte a range response starts, saying so in Content-Range. */
    var rangeSkew = 0

    /** One-page PDFs by page number, for `/page/{n}`. */
    val pages = mutableMapOf<Int, ByteArray>()

    /** Covers by BookFile id. */
    val covers = mutableMapOf<String, ByteArray>()

    /** Every orphan report's body. */
    val orphanReports = mutableListOf<String>()

    /** Every mark pushed, in order, across every push that got through. */
    val pushedMarks: List<PushedMark>
        get() = pushed.flatMap { wire.decodeFromString<SyncPush>(it).annotations.orEmpty() }

    private val syncPulls get() = seen.filter { it.url.encodedPath == "/api/v1/sync" && it.method == HttpMethod.Get }

    /** The `since` of every sync pull, null for none. */
    val pulledSince: List<String?> get() = syncPulls.map { it.url.parameters["since"] }

    /** The `cursor` of every sync pull, null for none. */
    val pulledCursor: List<String?> get() = syncPulls.map { it.url.parameters["cursor"] }

    private fun syncPage(request: HttpRequestData, rows: List<String>): Triple<HttpStatusCode, String, ByteArray> {
        val cursor = request.url.parameters["cursor"]
        if (cursor != null && cursor == failPullAt) {
            return Triple(HttpStatusCode.InternalServerError, "application/json", """{"error":{"code":"internal","message":"boom"}}""".toByteArray())
        }
        val from = cursor?.removePrefix("after-")?.toInt() ?: 0
        val limit = request.url.parameters["limit"]?.toInt() ?: 500
        val to = minOf(rows.size, from + limit)
        val more = to < rows.size
        val (status, body) = pullPage(until, more, rows.subList(from, to), cursor = if (more) "after-$to" else null)
        return Triple(status, "application/json", body.toByteArray())
    }

    private val wire = Json { ignoreUnknownKeys = true }

    private fun pushAnswer(body: String): Triple<HttpStatusCode, String, ByteArray> {
        val json = "application/json"
        val marks = wire.decodeFromString<SyncPush>(body).annotations.orEmpty()
        val results = marks.joinToString(",") { """{"id":"${it.id}","status":"${markStatus[it.id] ?: "applied"}"}""" }
        return Triple(HttpStatusCode.OK, json, """{"marks":[$results]}""".toByteArray())
    }

    fun assetPath(name: String) = "/api/v1/books/$bookFileId/assets/$name"

    /** Headers beyond the content type, for the one response that has any. */
    private var extraHeaders: Map<String, String> = emptyMap()

    private fun libraryPage(request: HttpRequestData): Triple<HttpStatusCode, String, ByteArray> {
        val cursor = request.url.parameters["cursor"]
        if (cursor != null && cursor == failLibraryAt) {
            return Triple(HttpStatusCode.InternalServerError, "application/json", """{"error":{"code":"internal","message":"boom"}}""".toByteArray())
        }
        val from = cursor?.removePrefix("after-")?.toInt() ?: 0
        val limit = request.url.parameters["limit"]?.toInt() ?: 48
        val to = minOf(library.size, from + limit)
        val next = if (to < library.size) "\"nextCursor\":\"after-$to\"," else ""
        return Triple(HttpStatusCode.OK, "application/json", """{$next"items":[${library.subList(from, to).joinToString(",")}]}""".toByteArray())
    }

    /** `/file` as http.ServeContent answers it: whole, a range, or 416 past the end. */
    private fun fileAnswer(request: HttpRequestData): Triple<HttpStatusCode, String, ByteArray> {
        val range = request.headers["Range"]?.removePrefix("bytes=")?.removeSuffix("-")?.toIntOrNull()
        val ifRange = request.headers["If-Range"]
        val whole = range == null || (ifRange != null && ifRange != lastModified)
        val cut = cutFileAfter
        cutFileAfter = null
        fun sent(bytes: ByteArray) = if (cut != null && cut < bytes.size) bytes.copyOf(cut) else bytes
        if (whole) {
            extraHeaders = mapOf("Content-Length" to file.size.toString(), "Last-Modified" to lastModified)
            return Triple(HttpStatusCode.OK, "application/pdf", sent(file))
        }
        val from = range!! + rangeSkew
        if (from >= file.size) return Triple(HttpStatusCode.RequestedRangeNotSatisfiable, "text/plain", ByteArray(0))
        val rest = file.copyOfRange(from, file.size)
        extraHeaders = mapOf(
            "Content-Range" to "bytes $from-${file.size - 1}/${file.size}",
            "Content-Length" to rest.size.toString(),
            "Last-Modified" to lastModified,
        )
        return Triple(HttpStatusCode.PartialContent, "application/pdf", sent(rest))
    }

    private fun answer(request: HttpRequestData): Triple<HttpStatusCode, String, ByteArray> {
        val path = request.url.encodedPath
        val json = "application/json"
        fun ok(body: String) = Triple(HttpStatusCode.OK, json, body.toByteArray())
        val notFound = Triple(HttpStatusCode.NotFound, json, """{"error":{"code":"not_found","message":"no"}}""".toByteArray())
        if (path in broken) return Triple(HttpStatusCode.InternalServerError, json, """{"error":{"code":"internal","message":"boom"}}""".toByteArray())
        refused[path]?.let { status ->
            return Triple(status, json, """{"error":{"code":"refused","message":"refused with ${status.value}"}}""".toByteArray())
        }
        return when {
            path == "/api/v1/library" -> libraryPage(request)
            path.endsWith("/info") -> ok(info)
            path.endsWith("/file") -> fileAnswer(request)
            path.contains("/page/") -> pages[path.substringAfterLast('/').toInt()]?.let { Triple(HttpStatusCode.OK, "application/pdf", it) } ?: notFound
            path.endsWith("/cover") -> covers[path.removeSuffix("/cover").substringAfterLast('/')]?.let { Triple(HttpStatusCode.OK, "image/jpeg", it) } ?: notFound
            path.endsWith("/annotations/orphaned") && request.method == HttpMethod.Post -> ok("""{"orphaned":0}""")
            path.endsWith("/manifest") -> ok(manifest)
            path.endsWith("/progress") -> Triple(progress.first, json, progress.second.toByteArray())
            path.contains("/spine/") -> {
                val index = path.substringAfterLast('/').toInt()
                val html = chapters[index] ?: "<p>chapter $index</p>"
                ok(kotlinx.serialization.json.buildJsonObject {
                    put("index", kotlinx.serialization.json.JsonPrimitive(index))
                    put("html", kotlinx.serialization.json.JsonPrimitive(html))
                }.toString())
            }
            path.contains("/assets/") -> assets[path]?.let { Triple(HttpStatusCode.OK, "image/png", it) }
                ?: Triple(HttpStatusCode.NotFound, json, """{"error":{"code":"not_found","message":"no"}}""".toByteArray())
            path == "/api/v1/sync" && request.method == HttpMethod.Get && syncRows != null -> syncPage(request, syncRows!!)
            path == "/api/v1/sync" && request.method == HttpMethod.Get -> pulls.removeFirstOrNull()
                ?.let { (status, body) -> Triple(status, json, body.toByteArray()) }
                ?: ok("""{"until":"$until","more":false,"annotations":[],"deleted":[],"positions":[]}""")
            path.endsWith("/annotations") -> ok(annotationList)
            else -> Triple(HttpStatusCode.NotFound, json, """{"error":{"code":"not_found","message":"no"}}""".toByteArray())
        }
    }

    /**
     * A client for this server, with the engine on the test's own scheduler so
     * advanceUntilIdle waits for requests too. On its default dispatcher it
     * answers on another thread, and every assertion would race it.
     */
    fun client(
        scope: TestScope,
        tokens: com.lexarii.app.api.TokenStore = InMemoryTokenStore("t", "http://books.local:8080"),
    ): LexariiClient {
        val config = MockEngineConfig().apply { dispatcher = StandardTestDispatcher(scope.testScheduler) }
        config.addHandler { request ->
            seen += request
            if (offline) throw java.io.IOException("no route to host")
            if (request.url.encodedPath == "/api/v1/sync") {
                (if (request.method == HttpMethod.Get) pullGate else pushGate)?.await()
            }
            if (request.url.encodedPath.contains("/page/")) pageGate?.await()
            if (request.url.encodedPath.endsWith("/annotations/orphaned")) {
                orphanReports += String(request.body.toByteArray())
            }
            extraHeaders = emptyMap()
            val (status, type, body) = if (request.url.encodedPath == "/api/v1/sync" && request.method == HttpMethod.Post) {
                val sent = String(request.body.toByteArray())
                val answer = pushAnswer(sent)
                if (answer.first == HttpStatusCode.OK) pushed += sent
                if (answer.first == HttpStatusCode.OK && losePushResponses > 0) {
                    losePushResponses--
                    Triple(HttpStatusCode.InternalServerError, "application/json", """{"error":{"code":"internal","message":"lost"}}""".toByteArray())
                } else {
                    answer
                }
            } else {
                answer(request)
            }
            val headers = io.ktor.http.Headers.build {
                append(HttpHeaders.ContentType, type)
                for ((name, value) in extraHeaders) append(name, value)
            }
            respond(ByteReadChannel(body), status, headers)
        }
        return LexariiClient("http://books.local:8080", tokens, MockEngine(config))
    }
}

/** This app's on-device storage in a directory, with its disk work on the test's scheduler. */
fun TestScope.device(dir: File) = Device(dir, StandardTestDispatcher(testScheduler))

/** One mark as the server writes it. */
fun annotationJson(
    id: String,
    bookFileId: String,
    updatedAt: String,
    note: String? = null,
    // When the version was made, which is what merging compares; the row
    // time is the server's and means nothing to it.
    editedAt: String = updatedAt,
    shared: Boolean = false,
    author: String? = null,
    start: String = "epub:0/p[2]:0",
    end: String = "epub:0/p[2]:9",
    orphaned: Boolean = false,
): String = kotlinx.serialization.json.buildJsonObject {
    fun field(key: String, value: Any?) = put(
        key,
        when (value) {
            null -> kotlinx.serialization.json.JsonNull
            is Boolean -> kotlinx.serialization.json.JsonPrimitive(value)
            else -> kotlinx.serialization.json.JsonPrimitive(value.toString())
        },
    )
    field("id", id)
    field("bookFileId", bookFileId)
    field("type", if (note != null) "note" else "highlight")
    field("start", start)
    field("end", end)
    field("selectedText", "some words")
    field("note", note)
    field("orphaned", orphaned)
    field("shared", shared)
    field("author", author)
    field("createdAt", "2026-09-01T00:00:00Z")
    field("updatedAt", updatedAt)
    field("editedAt", editedAt)
}.toString()

/** One book on the shelf, as the library listing writes it. */
fun libraryItemJson(bookFileId: String, title: String = "Book $bookFileId", cover: Boolean = false, formats: String = "\"epub\""): String =
    """{"workId":"w-$bookFileId","title":"$title","authors":[],"formats":[$formats],"status":"ready","missing":false,""" +
        """"bookFileId":"$bookFileId","addedAt":"2026-09-01T00:00:00Z","canDownload":true""" +
        (if (cover) ""","coverUrl":"/api/v1/books/$bookFileId/cover"""" else "") + "}"

/** One page of a sync pull. */
fun pullPage(
    until: String,
    more: Boolean,
    annotations: List<String> = emptyList(),
    deleted: List<String> = emptyList(),
    positions: List<String> = emptyList(),
    cursor: String? = null,
): Pair<HttpStatusCode, String> = HttpStatusCode.OK to
    """{"until":"$until","more":$more,${cursor?.let { "\"cursor\":\"$it\"," } ?: ""}"annotations":[${annotations.joinToString(",")}],""" +
    """"deleted":[${deleted.joinToString(",")}],"positions":[${positions.joinToString(",")}]}"""
