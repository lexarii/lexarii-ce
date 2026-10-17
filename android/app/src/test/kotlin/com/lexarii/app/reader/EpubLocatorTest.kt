// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

/** The same wire form the server writes and the web reader reads. */
class EpubLocatorTest {

    @Test
    fun `a position the server wrote parses`() {
        assertEquals(EpubLocator(3, "div[1]/p[5]", 0), EpubLocator.parse("epub:3/div[1]/p[5]:0"))
        assertEquals(EpubLocator(0, "p[12]", 41), EpubLocator.parse("epub:0/p[12]:41"))
    }

    /** Everything the Go `ParseLocator` refuses, refused here too. */
    @Test
    fun `anything malformed is null rather than a guess`() {
        val refused = listOf(
            null,
            "",
            "pdf:12:0",
            "epub:3:0",
            "epub:3/:0",
            "epub:-1/p[1]:0",
            "epub:x/p[1]:0",
            "epub:3/p[1]:x",
            "epub:3/p[1]",
            "epub:3/a:b/p[1]:0",
            "epub:99999999999/p[1]:0",
        )
        for (value in refused) assertNull(value, EpubLocator.parse(value))
    }
}
