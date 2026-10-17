// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

/**
 * The note being written in the sheet, and the note it was written over.
 *
 * Saving stamps a new edit time, and the newest edit wins on every device -
 * so saving text nobody typed is not harmless. A sheet opened a moment
 * before the pull brought the laptop's newer note, then swiped away, would
 * otherwise send the old note back as the newest word and lose the laptop's
 * everywhere. Only what was typed is ever saved.
 *
 * `basis` is the note the draft started from; `text` is what the field holds.
 * `changedElsewhere` says the note moved underneath text somebody had typed:
 * theirs is kept, because they typed it, and they are told.
 */
data class NoteDraft(val basis: String, val text: String = basis, val changedElsewhere: Boolean = false) {
    /** Whether anything was typed. */
    val typed: Boolean get() = text != basis

    fun edited(text: String): NoteDraft = copy(text = text)

    /**
     * The mark's note as it is now. Untouched, the draft follows it; typed
     * over, it stays as typed and remembers that the note moved.
     */
    fun noteNow(note: String): NoteDraft = when {
        // Back to what the draft started from: whatever changed elsewhere has
        // been undone there, and there is nothing to warn about any more.
        note == basis -> copy(changedElsewhere = false)
        !typed -> NoteDraft(note)
        // Typed to exactly what arrived: nothing left to say.
        text == note -> NoteDraft(note)
        else -> copy(changedElsewhere = true)
    }

    /** What leaving the sheet should save, or null for nothing. */
    fun toSave(): String? = text.takeIf { typed }

    /** As a list of strings, for rememberSaveable across a rotation. */
    fun toSaved(): List<String> = listOf(basis, text, changedElsewhere.toString())

    companion object {
        fun fromSaved(saved: List<String>): NoteDraft = NoteDraft(saved[0], saved[1], saved[2].toBoolean())
    }
}
