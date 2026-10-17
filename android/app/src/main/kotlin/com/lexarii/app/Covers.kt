// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app

import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.util.LruCache
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import com.lexarii.app.api.ServerRefused
import com.lexarii.app.api.LexariiClient
import com.lexarii.app.offline.CoverStore
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Semaphore
import kotlinx.coroutines.sync.withPermit
import kotlinx.coroutines.withContext
import java.io.ByteArrayOutputStream
import java.util.Collections

/**
 * Covers for the shelf, with no image library: the platform decodes them.
 *
 * A cover needs the bearer token, so it is fetched by the app's own client,
 * never by a URL handed to an image loader. What is kept on disk is a
 * thumbnail the size the shelf draws it, not the server's full image - a
 * library of a thousand covers is then a few megabytes, not a few hundred.
 *
 * The bytes are untrusted, like everything that came out of a book. Their
 * size is read first and the decode scaled down to fit, so a cover claiming
 * to be enormous costs a few kilobytes of memory, not the app.
 */
class Covers(private val store: CoverStore, private val client: () -> LexariiClient?) {
    private val memory = object : LruCache<String, ImageBitmap>(MEMORY_BYTES) {
        override fun sizeOf(key: String, value: ImageBitmap) = value.width * value.height * 4
    }

    // Covers the server has said it does not have, for this run: not asked
    // again every time a row scrolls back into view.
    private val absent = Collections.synchronizedSet(mutableSetOf<String>())

    // A few at a time: a fling through the shelf starts a request per row.
    private val gate = Semaphore(3)

    fun cached(bookFileId: String): ImageBitmap? = memory.get(bookFileId)

    /** The cover, from memory, disk or the server - or null for none, or none reachable. */
    suspend fun load(bookFileId: String): ImageBitmap? {
        memory.get(bookFileId)?.let { return it }
        val thumbnail = store.get(bookFileId) ?: fetch(bookFileId) ?: return null
        val image = withContext(Dispatchers.Default) { decode(thumbnail, THUMB_WIDTH * 2) }?.asImageBitmap() ?: return null
        memory.put(bookFileId, image)
        return image
    }

    fun forget() {
        memory.evictAll()
        absent.clear()
    }

    private suspend fun fetch(bookFileId: String): ByteArray? {
        if (bookFileId in absent) return null
        val connection = client() ?: return null
        val raw = try {
            gate.withPermit { connection.cover(bookFileId) }.bytes
        } catch (e: CancellationException) {
            throw e
        } catch (e: ServerRefused) {
            // Only "there is none" is remembered. A server error or a refusal
            // of the moment is asked again the next time the row is drawn.
            if (e.status == 404 || e.status == 410) absent += bookFileId
            return null
        } catch (e: Exception) {
            return null // no network: the next launch can try again
        }
        val thumbnail = withContext(Dispatchers.Default) { shrink(raw) }
        if (thumbnail == null) {
            absent += bookFileId
            return null
        }
        store.put(bookFileId, thumbnail)
        return thumbnail
    }

    private companion object {
        // Twice the 48dp the shelf draws a cover at, for a sharp picture on
        // a dense screen; kept a little larger than it is drawn.
        const val THUMB_WIDTH = 160
        const val MEMORY_BYTES = 12 * 1024 * 1024

        // Past this many pixels after scaling, the shape is not a cover's -
        // a sliver a million pixels tall - and it is not drawn.
        const val MAX_DECODED_PIXELS = 4_000_000

        /** A JPEG thumbnail `THUMB_WIDTH` wide, or null if these bytes are not a usable image. */
        fun shrink(bytes: ByteArray): ByteArray? {
            val bitmap = decode(bytes, THUMB_WIDTH) ?: return null
            val scaled = if (bitmap.width > THUMB_WIDTH) {
                val height = (bitmap.height.toLong() * THUMB_WIDTH / bitmap.width).toInt().coerceAtLeast(1)
                Bitmap.createScaledBitmap(bitmap, THUMB_WIDTH, height, true).also { if (it !== bitmap) bitmap.recycle() }
            } else {
                bitmap
            }
            val out = ByteArrayOutputStream()
            val ok = scaled.compress(Bitmap.CompressFormat.JPEG, 85, out)
            scaled.recycle()
            return if (ok) out.toByteArray() else null
        }

        /** Decoded no larger than needed for `width`, having read the size first. */
        fun decode(bytes: ByteArray, width: Int): Bitmap? {
            val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
            BitmapFactory.decodeByteArray(bytes, 0, bytes.size, bounds)
            if (bounds.outWidth <= 0 || bounds.outHeight <= 0) return null
            var sample = 1
            while (bounds.outWidth / (sample * 2) >= width) sample *= 2
            val pixels = (bounds.outWidth.toLong() / sample) * (bounds.outHeight.toLong() / sample)
            if (pixels > MAX_DECODED_PIXELS) return null
            val options = BitmapFactory.Options().apply {
                inSampleSize = sample
                inPreferredConfig = Bitmap.Config.ARGB_8888
            }
            return runCatching { BitmapFactory.decodeByteArray(bytes, 0, bytes.size, options) }.getOrNull()
        }
    }
}
