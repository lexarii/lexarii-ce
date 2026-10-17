// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app

import android.content.Context
import com.lexarii.app.offline.Device

/**
 * The one Device in this process.
 *
 * One, because the screen and a download running in the background both
 * write under the same directory, and each store's locks - and the flag that
 * shuts a store when somebody signs out - only mean anything if everybody
 * holds the same store.
 */
object Services {
    @Volatile
    private var device: Device? = null

    fun device(context: Context): Device =
        device ?: synchronized(this) {
            device ?: context.applicationContext.let { Device(it.filesDir, cacheDir = it.cacheDir) }.also { device = it }
        }
}
