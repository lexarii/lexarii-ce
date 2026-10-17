// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.offline

import com.lexarii.app.api.Asset
import com.lexarii.app.api.BookInfo
import com.lexarii.app.api.ReaderManifest
import com.lexarii.app.api.LexariiClient
import com.lexarii.app.api.SpineContent
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.io.File
import java.io.FileOutputStream
import java.io.IOException
import kotlin.coroutines.coroutineContext

/**
 * Books kept on this device, one directory per BookFile.
 *
 * An EPUB is kept as exactly the responses the reader would have fetched, so
 * reading from here is reading the same chapters, sanitised by the same server:
 *
 *     <root>/<bookFileId>/manifest.json
 *                         spine/<index>.json
 *                         assets/<sha256 of the asset's path>
 *                         assets/<sha256>.type
 *
 * A PDF is kept as the file itself, because a PDF is fixed pages and the
 * platform renders them straight from it:
 *
 *     <root>/<bookFileId>/manifest.json
 *                         info.json
 *                         file.pdf
 *
 * Only the directory name comes from the server, and only after it has been
 * checked to be a plain id; an asset's path is hashed rather than trusted as a
 * filename.
 *
 * A copy is written into `.partial-<id>` and renamed into place when complete,
 * so a download that dies halfway is never mistaken for a copy - and is not
 * thrown away either. A download that is started again picks up from what the
 * partial holds: the chapters already fetched, or the bytes of the file.
 */
class OfflineStore(
    private val root: File,
    // Injectable so a test can run the disk work on its own scheduler.
    private val io: CoroutineDispatcher = Dispatchers.IO,
) {
    private val json = Json { ignoreUnknownKeys = true; explicitNulls = false }

    /**
     * How many times this store has been cleared. A download remembers the
     * count it began under and stops writing the moment it changes: a
     * download still running when the account signs out - in the background,
     * where nothing waits for it - must never write that account's book into
     * the next one's storage, even after the next sign-in has opened it.
     */
    private var epoch = 0

    /**
     * How many times each BookFile's copy has been removed - the person
     * discarding it, or stopping its download. A download that began before
     * stops writing the moment it changes, or its last few writes, landing
     * after the removal, would put a chapter-less copy back as "kept".
     */
    private val removals = mutableMapOf<String, Int>()

    // Held for every write a download makes, and for clearing and removing:
    // a check that the download may still write and the write itself happen
    // together, never with a removal between them.
    private val lock = Mutex()

    /** A download in progress: its partial directory, and the clear and removal it must not outlive. */
    private inner class Work(val dir: File, val bookFileId: String, private val began: Int, private val removed: Int) {
        /** Runs `block` on the disk, but only if nothing has cleared or removed this copy since it began. */
        suspend fun <T> guarded(block: () -> T): T = lock.withLock {
            withContext(io) {
                if (epoch != began) throw IOException("signed out")
                if ((removals[bookFileId] ?: 0) != removed) throw IOException("the download was stopped")
                block()
            }
        }
    }

    fun has(bookFileId: String): Boolean = File(dir(bookFileId), MANIFEST).isFile

    /** Every BookFile with a complete copy here. */
    fun kept(): Set<String> =
        root.listFiles().orEmpty()
            .filter { it.isDirectory && !it.name.startsWith(".") && File(it, MANIFEST).isFile }
            .map { it.name }
            .toSet()

    suspend fun manifest(bookFileId: String): ReaderManifest? = read(File(dir(bookFileId), MANIFEST))

    suspend fun spine(bookFileId: String, index: Int): SpineContent? = read(File(dir(bookFileId), "spine/$index.json"))

    /** What the server said about a kept PDF when it was kept: its page count, chiefly. */
    suspend fun info(bookFileId: String): BookInfo? = read(File(dir(bookFileId), INFO))

    /** A kept PDF's file, for the renderer, or null if this BookFile has none. */
    suspend fun pdf(bookFileId: String): File? = withContext(io) {
        File(dir(bookFileId), PDF).takeIf { it.isFile && File(dir(bookFileId), MANIFEST).isFile }
    }

    suspend fun asset(bookFileId: String, path: String): Asset? = withContext(io) {
        val name = hashed(path)
        val bytes = File(dir(bookFileId), "assets/$name")
        val type = File(dir(bookFileId), "assets/$name.type")
        if (bytes.isFile && type.isFile) Asset(type.readText(), bytes.readBytes()) else null
    }

    /**
     * Deletes a copy, and any half-downloaded one: this is the person saying
     * they do not want it. A download of it still running stops writing.
     */
    suspend fun remove(bookFileId: String) = lock.withLock {
        val (target, work) = dir(bookFileId) to partial(bookFileId)
        removals[bookFileId] = (removals[bookFileId] ?: 0) + 1
        withContext(io) {
            target.deleteRecursively()
            work.deleteRecursively()
        }
    }

    /** Everything, for signing out. Every download already running stops writing. */
    suspend fun clear() = lock.withLock {
        epoch++
        withContext(io) { root.deleteRecursively() }
    }

    /**
     * Fetches a whole book and keeps it: an EPUB chapter by chapter, a PDF as
     * its file.
     *
     * `progress` hears (done, total, inBytes): chapters for an EPUB, bytes for
     * a PDF. Cancelled or failed, it stops between requests and leaves the
     * partial copy where it is, to be carried on from next time - nothing half
     * done is ever shown as kept. Anything but an EPUB or a PDF is refused
     * before anything is fetched.
     */
    suspend fun download(
        client: LexariiClient,
        bookFileId: String,
        progress: (done: Long, total: Long, inBytes: Boolean) -> Unit = { _, _, _ -> },
    ) {
        val work = lock.withLock { Work(partial(bookFileId), bookFileId, epoch, removals[bookFileId] ?: 0) }
        val manifest = client.manifest(bookFileId)
        when (manifest.format) {
            "epub" -> {
                require(manifest.spine.isNotEmpty()) { "This book has no chapters to keep" }
                keepEpub(client, bookFileId, manifest, work, progress)
            }
            "pdf" -> keepPdf(client, bookFileId, manifest, work, progress)
            else -> {
                work.guarded { work.dir.deleteRecursively() }
                throw IllegalArgumentException("Only EPUBs and PDFs can be kept on the device")
            }
        }
        // Last, because its presence is what "complete" means.
        write(work, MANIFEST, json.encodeToString(manifest))
        // The check and the rename together: a removal cannot fall between them.
        work.guarded {
            for (name in listOf(STARTED, VALIDATOR, LENGTH)) File(work.dir, name).delete()
            val target = dir(bookFileId)
            target.deleteRecursively()
            if (!work.dir.renameTo(target)) throw IOException("could not keep the download")
        }
    }

    private suspend fun keepEpub(
        client: LexariiClient,
        bookFileId: String,
        manifest: ReaderManifest,
        work: Work,
        progress: (Long, Long, Boolean) -> Unit,
    ) {
        startOrResume(work, manifest)
        val total = manifest.spine.size.toLong()
        val fetched = mutableSetOf<String>()
        progress(0, total, false)
        for ((done, item) in manifest.spine.withIndex()) {
            coroutineContext.ensureActive()
            val name = "spine/${item.index}.json"
            // A chapter already here from an earlier try is not asked for
            // again. Every file is written whole, so one that is here is one
            // that was finished.
            val content = read<SpineContent>(File(work.dir, name)) ?: client.spine(bookFileId, item.index).also {
                write(work, name, json.encodeToString(it))
            }
            for (path in assetPaths(content.html, bookFileId)) {
                if (!fetched.add(path)) continue
                val hash = hashed(path)
                val bytes = File(work.dir, "assets/$hash")
                val type = File(work.dir, "assets/$hash.type")
                if (withContext(io) { bytes.isFile && type.isFile }) continue
                coroutineContext.ensureActive()
                val asset = try {
                    client.asset(bookFileId, path)
                } catch (e: IllegalArgumentException) {
                    continue // not one of this book's images; the reader would refuse it too
                }
                work.guarded {
                    // The bytes before the type: both are there only once both are whole.
                    bytes.writeAtomically(asset.bytes)
                    type.writeAtomically(asset.contentType.toByteArray())
                }
            }
            progress(done + 1L, total, false)
        }
    }

    private suspend fun keepPdf(
        client: LexariiClient,
        bookFileId: String,
        manifest: ReaderManifest,
        work: Work,
        progress: (Long, Long, Boolean) -> Unit,
    ) {
        val info = client.info(bookFileId)
        require(info.format == "pdf") { "The server describes this book differently from its contents" }
        // The file itself is what is kept, and a book lent to be read is
        // never handed over as a file.
        require(info.canDownload) { "This book was shared to be read, not kept" }
        startOrResume(work, manifest)
        write(work, INFO, json.encodeToString(info))

        val file = File(work.dir, PDF)
        val validatorFile = File(work.dir, VALIDATOR)
        val lengthFile = File(work.dir, LENGTH)
        val (held, validator) = work.guarded {
            val v = validatorFile.takeIf { it.isFile }?.readText()?.takeIf { it.isNotBlank() }
            // Bytes with nothing to check them against cannot be resumed:
            // there is no telling whether they are this file's.
            if (v == null || !lengthFile.isFile) {
                file.delete()
                validatorFile.delete()
                lengthFile.delete()
            }
            (if (file.isFile) file.length() else 0L) to v?.takeIf { lengthFile.isFile }
        }
        val heldOf = if (held > 0) work.guarded { lengthFile.readText().toLongOrNull() } else null
        var total = info.sizeBytes.takeIf { it > 0 } ?: 0L
        var done = held
        progress(done, total, true)

        var out: FileOutputStream? = null
        try {
            client.download(
                bookFileId,
                from = held,
                validator = validator,
                begin = { started ->
                    // Last-Modified is to the second, and a file replaced by a
                    // copy that keeps its time (rsync -a, cp -p) passes it. The
                    // whole length has to match too, or the rest of a new file
                    // would be spliced onto the start of the old one.
                    if (!started.restarted && heldOf != null && started.total != null && started.total != heldOf) {
                        work.guarded {
                            file.delete()
                            validatorFile.delete()
                            lengthFile.delete()
                        }
                        throw IOException("the file changed on the server; it will be fetched again from the start")
                    }
                    started.total?.let { total = it }
                    if (started.restarted) done = 0
                    work.guarded {
                        // Truncated when the server sends it all again.
                        out = FileOutputStream(file, !started.restarted)
                        if (started.restarted) {
                            // What these bytes are, for checking them next time.
                            if (started.validator != null && started.total != null) {
                                validatorFile.writeAtomically(started.validator.toByteArray())
                                lengthFile.writeAtomically(started.total.toString().toByteArray())
                            } else {
                                validatorFile.delete()
                                lengthFile.delete()
                            }
                        }
                    }
                    progress(done, total, true)
                },
                sink = { buffer, count ->
                    work.guarded { out?.write(buffer, 0, count) }
                    done += count
                    progress(done, total, true)
                },
            )
        } finally {
            withContext(NonCancellable + io) {
                out?.let { stream ->
                    // On disk before anything counts it as held: a resume
                    // trusts the file's length.
                    runCatching { stream.fd.sync() }
                    stream.close()
                }
            }
        }

        work.guarded {
            val length = file.length()
            // Short means the connection ended early, which on a phone is the
            // ordinary way a download stops. What is here is kept, to resume.
            if (total > 0 && length < total) throw IOException("the download stopped at $length of $total bytes")
            // Longer, or not a PDF, is not this file: start again next time.
            if ((total > 0 && length > total) || !startsWithPdfMagic(file)) {
                file.delete()
                validatorFile.delete()
                lengthFile.delete()
                throw IOException("the downloaded file is not the PDF the server described")
            }
        }
    }

    /**
     * Starts a partial copy, or carries on the one there - unless the book
     * has changed since it was started, when the old pieces would not fit
     * the new book and are thrown away.
     */
    private suspend fun startOrResume(work: Work, manifest: ReaderManifest) {
        val before = read<ReaderManifest>(File(work.dir, STARTED))
        if (before != manifest) {
            work.guarded { work.dir.deleteRecursively() }
        }
        write(work, STARTED, json.encodeToString(manifest))
    }

    private suspend inline fun <reified T> read(file: File): T? = withContext(io) {
        if (!file.isFile) return@withContext null
        runCatching { json.decodeFromString<T>(file.readText()) }.getOrNull()
    }

    private suspend fun write(work: Work, name: String, text: String) = work.guarded {
        File(work.dir, name).writeAtomically(text.toByteArray())
    }

    private fun dir(bookFileId: String) = File(root, checked(bookFileId))
    private fun partial(bookFileId: String) = File(root, ".partial-" + checked(bookFileId))

    private fun checked(bookFileId: String): String {
        require(ID.matches(bookFileId)) { "not a book file id: $bookFileId" }
        return bookFileId
    }

    companion object {
        private const val MANIFEST = "manifest.json"
        private const val INFO = "info.json"
        private const val PDF = "file.pdf"
        // What the partial copy was started from, and the server's
        // Last-Modified for the bytes of a PDF so far. Neither survives into
        // the finished copy.
        private const val STARTED = "started.json"
        private const val VALIDATOR = "validator"
        // The whole file's length as the server first gave it, beside the validator.
        private const val LENGTH = "length"
        private val ID = Regex("^[0-9A-Za-z-]{1,64}$")
        private val IMG_SRC = Regex("""<img\b[^>]*?\bsrc="([^"]*)"""", RegexOption.IGNORE_CASE)

        /**
         * The image paths a chapter uses, as the WebView will ask for them.
         *
         * The server writes them as absolute paths under this book's assets;
         * anything else - a data: URI, a blocked image - is not fetched.
         */
        fun assetPaths(html: String, bookFileId: String): List<String> {
            val prefix = "/api/v1/books/$bookFileId/assets/"
            return IMG_SRC.findAll(html)
                .map { unescape(it.groupValues[1]) }
                .filter { it.startsWith(prefix) }
                .distinct()
                .toList()
        }

        // The attribute is HTML; the request the WebView makes is not.
        private fun unescape(value: String) = value
            .replace("&lt;", "<").replace("&gt;", ">").replace("&quot;", "\"")
            .replace("&#39;", "'").replace("&#34;", "\"").replace("&amp;", "&")

        private fun startsWithPdfMagic(file: File): Boolean {
            val magic = ByteArray(5)
            val read = file.inputStream().use { it.read(magic) }
            return read == 5 && String(magic, Charsets.US_ASCII) == "%PDF-"
        }
    }
}
