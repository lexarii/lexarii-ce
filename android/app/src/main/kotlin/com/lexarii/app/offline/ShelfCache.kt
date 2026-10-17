// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.offline

import com.lexarii.app.api.LibraryItem
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.io.File

/**
 * The last shelf the server sent, so a launch with no network still shows the
 * library - and the copies kept on the device can still be opened from it.
 */
class ShelfCache(
    private val file: File,
    // Injectable so a test can run the disk work on its own scheduler.
    private val io: CoroutineDispatcher = Dispatchers.IO,
) {
    @Serializable
    data class Shelf(val items: List<LibraryItem>, val syncedAt: String? = null)

    private val json = Json { ignoreUnknownKeys = true; explicitNulls = false }
    private val lock = Mutex()

    // Set by clear() until reopen(): a library load still in flight for the
    // account that signed out must not save its shelf for the next one.
    private var closed = false

    suspend fun load(): Shelf? = withContext(io) {
        if (!file.isFile) return@withContext null
        runCatching { json.decodeFromString<Shelf>(file.readText()) }.getOrNull()
    }

    suspend fun save(shelf: Shelf) = lock.withLock {
        if (closed) return@withLock
        withContext(io) { file.writeAtomically(json.encodeToString(shelf).toByteArray()) }
    }

    suspend fun clear() = lock.withLock {
        closed = true
        withContext(io) { file.delete() }
    }

    suspend fun reopen() = lock.withLock { closed = false }
}
