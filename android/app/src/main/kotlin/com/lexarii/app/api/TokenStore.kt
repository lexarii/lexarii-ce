// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.api

import android.content.Context
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import kotlinx.coroutines.flow.first

/**
 * Where the bearer token lives between runs.
 *
 * An interface, because the client needs somewhere to keep a token and not an
 * Android dependency: the tests hand it one that holds a string in memory,
 * which is what lets the whole client be tested on a JVM with no device.
 *
 * The server address is kept beside the token and saved in the same write,
 * because a token means nothing to any server but the one that issued it.
 * Clearing drops the token and keeps the address: somebody signed out is
 * almost always signing back in to the same library.
 */
interface TokenStore {
    suspend fun token(): String?
    suspend fun server(): String?
    suspend fun save(token: String, server: String)
    suspend fun clear()
}

/** A token held only for as long as the object is, for tests. */
class InMemoryTokenStore(
    private var value: String? = null,
    private var address: String? = null,
) : TokenStore {
    override suspend fun token(): String? = value
    override suspend fun server(): String? = address
    override suspend fun save(token: String, server: String) {
        value = token
        address = server
    }
    override suspend fun clear() { value = null }
}

private val Context.tokenDataStore by preferencesDataStore(name = "lexarii-session")

/**
 * The real one: DataStore, in the app's private directory.
 *
 * Not encrypted, and the reason is worth stating rather than leaving to look
 * like an oversight. The app's private directory is already unreadable to
 * other apps, and androidx.security-crypto - the usual answer - is deprecated
 * and would have to be justified to F-Droid as a dependency that buys little
 * against an attacker who has root, which is the only attacker it would stop.
 * The token is revocable from any other device, which is the mitigation that
 * actually matters when a phone is lost.
 */
class DataStoreTokenStore(private val context: Context) : TokenStore {
    override suspend fun token(): String? =
        context.tokenDataStore.data.first()[KEY]

    override suspend fun server(): String? =
        context.tokenDataStore.data.first()[SERVER]

    override suspend fun save(token: String, server: String) {
        context.tokenDataStore.edit {
            it[KEY] = token
            it[SERVER] = server
        }
    }

    override suspend fun clear() {
        context.tokenDataStore.edit { it.remove(KEY) }
    }

    private companion object {
        val KEY = stringPreferencesKey("token")
        val SERVER = stringPreferencesKey("server")
    }
}
