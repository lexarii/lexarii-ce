// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Leaving the note sheet saves what was typed, and nothing else: a save
 * stamps a new edit time, and the newest edit wins on every device.
 */
class NoteDraftTest {
    /** Tapped a mark, a pull brought the laptop's newer note, swiped away. */
    @Test
    fun `an untouched draft follows the note and saves nothing`() {
        val draft = NoteDraft("old note from the phone").noteNow("newer note from the laptop")
        assertEquals("newer note from the laptop", draft.text)
        assertFalse(draft.typed)
        assertFalse(draft.changedElsewhere)
        assertNull("leaving must not send the old note back as the newest", draft.toSave())
    }

    @Test
    fun `a draft nobody touched saves nothing`() {
        assertNull(NoteDraft("a note").toSave())
        assertNull(NoteDraft("").toSave())
    }

    @Test
    fun `what was typed is saved`() {
        val draft = NoteDraft("a note").edited("a better note")
        assertTrue(draft.typed)
        assertEquals("a better note", draft.toSave())
        // Cleared on purpose is typed too.
        assertEquals("", NoteDraft("a note").edited("").toSave())
    }

    /** Both changed: theirs arrived while this person was writing. Kept, because they typed it, and said. */
    @Test
    fun `typed text is kept when the note changes underneath, and flagged`() {
        val draft = NoteDraft("a note").edited("my rewrite").noteNow("the laptop's rewrite")
        assertEquals("my rewrite", draft.text)
        assertTrue(draft.changedElsewhere)
        assertEquals("my rewrite", draft.toSave())
    }

    @Test
    fun `typed to exactly what arrived is nothing to save`() {
        val draft = NoteDraft("a note").edited("same words").noteNow("same words")
        assertFalse(draft.typed)
        assertFalse(draft.changedElsewhere)
        assertNull(draft.toSave())
    }

    @Test
    fun `the note arriving again unchanged changes nothing`() {
        val draft = NoteDraft("a note").edited("typing")
        assertEquals(draft, draft.noteNow("a note"))
    }

    @Test
    fun `a draft survives being saved and restored across a rotation`() {
        val draft = NoteDraft("a note").edited("typing, \"quoted\"").noteNow("elsewhere")
        assertEquals(draft, NoteDraft.fromSaved(draft.toSaved()))
    }

    /** Changed elsewhere and then changed back: nothing left to warn about. */
    @Test
    fun `the warning clears when the note changes back to what the draft started from`() {
        val warned = NoteDraft("a note").edited("my rewrite").noteNow("the laptop's rewrite")
        assertTrue(warned.changedElsewhere)
        val undone = warned.noteNow("a note")
        assertFalse(undone.changedElsewhere)
        assertEquals("my rewrite", undone.text)
        assertEquals("still saved: it was typed", "my rewrite", undone.toSave())
    }
}
