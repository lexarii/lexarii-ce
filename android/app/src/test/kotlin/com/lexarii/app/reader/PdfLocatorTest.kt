// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

/** The same answers as `ParseLocator` in internal/domain/locator.go, or stricter. */
class PdfLocatorTest {
    @Test
    fun `a PDF position is read back`() {
        assertEquals(PdfLocator(12, 0), PdfLocator.parse("pdf:12:0"))
        assertEquals(PdfLocator(1, 340), PdfLocator.parse("pdf:1:340"))
        assertEquals("pdf:12:0", PdfLocator(12).format())
        // Leading zeros are digits to Go too; written back canonically.
        assertEquals("pdf:7:0", PdfLocator.parse("pdf:007:0")?.format())
    }

    @Test
    fun `anything Go refuses is refused`() {
        for (bad in listOf(
            null,
            "",
            "pdf:",
            "pdf:3",
            // Go: "page 0 is not one-based".
            "pdf:0:0",
            "pdf:-1:0",
            "pdf:3:-1",
            // Go's offset after the page is "0:1", which Atoi refuses.
            "pdf:3:0:1",
            "pdf:a:0",
            "pdf:3:b",
            "pdf: 3:0",
            // Overflows an int.
            "pdf:99999999999:0",
            "pdf:3:99999999999",
            "PDF:3:0",
            "epub:0/p[1]:0",
        )) {
            assertNull("accepted $bad", PdfLocator.parse(bad))
        }
    }

    /** Go's Atoi takes a sign; nothing in Lexarii writes one, so it is refused here too. */
    @Test
    fun `a signed page is refused, which is stricter than Go`() {
        assertNull(PdfLocator.parse("pdf:+3:0"))
    }
}
