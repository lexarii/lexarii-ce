// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

/**
 * A PDF position in the one wire form every part of Lexarii uses:
 * `pdf:<page>:<charOffset>`, the page one-based.
 *
 * The app writes `pdf:<page>:0` - a PDF's pages are fixed, so the page is the
 * whole of where somebody is - and reads any well-formed one back to resume.
 *
 * Mirrors `ParseLocator` in internal/domain/locator.go and `pageFromLocator`
 * in the web's PDF reader; anything either would refuse is refused here. It
 * is stricter than Go in one respect, as EpubLocator is: Go's Atoi takes a
 * leading sign, and no Lexarii ever writes one.
 */
data class PdfLocator(val page: Int, val charOffset: Int = 0) {
    init {
        require(page >= 1) { "pages are one-based" }
        require(charOffset >= 0) { "a char offset is never negative" }
    }

    /** The canonical wire form, as the server would write it back. */
    fun format(): String = "pdf:$page:$charOffset"

    companion object {
        private val FORM = Regex("""^pdf:(\d+):(\d+)$""")

        /** The locator, or null for anything malformed or not a PDF position. */
        fun parse(value: String?): PdfLocator? {
            val match = FORM.matchEntire(value ?: return null) ?: return null
            val (page, offset) = match.destructured
            // Digits the regex accepted can still overflow an Int.
            val p = page.toIntOrNull() ?: return null
            val o = offset.toIntOrNull() ?: return null
            // Go: "page %d is not one-based".
            if (p < 1) return null
            return PdfLocator(p, o)
        }
    }
}
