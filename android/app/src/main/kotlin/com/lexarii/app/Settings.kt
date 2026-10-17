// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app

import android.content.Context
import androidx.datastore.core.DataStore
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.booleanPreferencesKey
import androidx.datastore.preferences.core.doublePreferencesKey
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import com.lexarii.app.reader.FontChoice
import com.lexarii.app.reader.MarginChoice
import com.lexarii.app.reader.ReadingSettings
import com.lexarii.app.reader.ThemeChoice
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.map

private val Context.settingsDataStore by preferencesDataStore(name = "lexarii-settings")

/**
 * This device's own choices: how the reader lays out text, and whether books
 * are kept over mobile data.
 *
 * Its own file, apart from the session's, and deliberately NOT cleared on
 * signing out: none of it belongs to an account, and a person signing back in
 * should not find their font size reset.
 */
class Settings(private val store: DataStore<Preferences>) {
    constructor(context: Context) : this(context.applicationContext.settingsDataStore)

    val reading: Flow<ReadingSettings> = store.data.map { p ->
        ReadingSettings(
            theme = ThemeChoice.of(p[THEME]),
            font = FontChoice.of(p[FONT]),
            fontScale = p[SCALE] ?: 1.0,
            lineHeight = p[LINE] ?: ReadingSettings.DEFAULT_LINE_HEIGHT,
            margin = MarginChoice.of(p[MARGIN]),
        ).normalised()
    }.distinctUntilChanged()

    /** Keep books only on a network that is not metered - Wi-Fi, as a rule. */
    val unmeteredOnly: Flow<Boolean> = store.data.map { it[UNMETERED] ?: false }.distinctUntilChanged()

    suspend fun save(settings: ReadingSettings) {
        val s = settings.normalised()
        store.edit {
            it[THEME] = s.theme.key
            it[FONT] = s.font.key
            it[SCALE] = s.fontScale
            it[LINE] = s.lineHeight
            it[MARGIN] = s.margin.key
        }
    }

    suspend fun setUnmeteredOnly(on: Boolean) {
        store.edit { it[UNMETERED] = on }
    }

    private companion object {
        val THEME = stringPreferencesKey("reader.theme")
        val FONT = stringPreferencesKey("reader.font")
        val SCALE = doublePreferencesKey("reader.fontScale")
        val LINE = doublePreferencesKey("reader.lineHeight")
        val MARGIN = stringPreferencesKey("reader.margin")
        val UNMETERED = booleanPreferencesKey("downloads.unmeteredOnly")
    }
}
