// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.offline

import java.io.File
import java.io.IOException
import java.security.MessageDigest

/**
 * Replaces a file in one step: written beside it, then renamed over it.
 *
 * A process killed mid-write - which on Android is simply what happens to
 * apps in the background - leaves the old file or the new one, never half of
 * either.
 */
internal fun File.writeAtomically(bytes: ByteArray) {
    parentFile?.mkdirs()
    val temp = File(parentFile, ".$name.tmp")
    temp.writeBytes(bytes)
    if (!temp.renameTo(this)) {
        temp.delete()
        throw IOException("could not replace $path")
    }
}

/** A name for something whose own name came from a book. Never a path. */
internal fun hashed(value: String): String =
    MessageDigest.getInstance("SHA-256").digest(value.toByteArray()).joinToString("") { "%02x".format(it) }
