// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

/**
 * An EPUB position in the one wire form every part of Lexarii uses:
 * `epub:<spineIndex>/<elementPath>:<charOffset>`.
 *
 * The app only ever reads one - to know which chapter to open when resuming.
 * Building them is the WebView's job, with the web reader's own code, because
 * an element path is a fact about a DOM and the DOM is over there.
 *
 * Mirrors `ParseLocator` in internal/domain/locator.go and `parseLocator` in
 * web/src/lib/reader/locator.ts; anything either would refuse is refused here.
 */
data class EpubLocator(val spineIndex: Int, val path: String, val charOffset: Int) {
    /** The canonical wire form, as the server would write it back. */
    fun format(): String = "epub:$spineIndex/$path:$charOffset"

    companion object {
        private val FORM = Regex("""^epub:(\d+)/(.+):(\d+)$""")

        /** The locator, or null for anything malformed or not an EPUB position. */
        fun parse(value: String?): EpubLocator? {
            val match = FORM.matchEntire(value ?: return null) ?: return null
            val (spine, path, offset) = match.destructured
            // The server's rule: the offset is found by the first ':' after
            // the prefix, so a path holding one is not a path it wrote.
            if (':' in path) return null
            // Digits the regex accepted can still overflow an Int.
            val index = spine.toIntOrNull() ?: return null
            val chars = offset.toIntOrNull() ?: return null
            return EpubLocator(index, path, chars)
        }
    }
}
