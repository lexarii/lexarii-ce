// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import com.lexarii.app.api.PushedPosition
import com.lexarii.app.api.ReadingProgress
import com.lexarii.app.api.LexariiClient
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.withTimeoutOrNull
import java.time.Instant

/**
 * Where to open a book: the newer of this device's own last position and the
 * server's, by when each was read.
 *
 * Somewhere to start is not worth failing over, and not worth waiting on
 * either when the book is already on the device: a kept copy opened with no
 * network gets a few seconds for the server to answer, then opens where this
 * device last was.
 */
internal suspend fun resumeFrom(
    local: PushedPosition?,
    client: LexariiClient,
    bookFileId: String,
    kept: Boolean,
    patienceMs: Long,
): String? {
    val server: ReadingProgress? = try {
        if (kept) withTimeoutOrNull(patienceMs) { client.progress(bookFileId) } else client.progress(bookFileId)
    } catch (e: CancellationException) {
        throw e
    } catch (e: Exception) {
        null
    }
    val serverAt = server?.lastReadAt?.let { runCatching { Instant.parse(it) }.getOrNull() }
    val localAt = local?.let { runCatching { Instant.parse(it.readAt) }.getOrNull() }
    val preferLocal = local != null && (server?.locator == null || serverAt == null || (localAt != null && localAt > serverAt))
    return if (preferLocal) local?.position else server?.locator
}
