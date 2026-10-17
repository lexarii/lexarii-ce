// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import java.security.SecureRandom
import java.util.Base64

/**
 * The page a chapter is drawn into, assembled from the three files in
 * `assets/reader/`.
 *
 * The scripts are inlined rather than loaded, so the page makes no request of
 * its own and needs no file access. Each load gets a fresh nonce, and the
 * page's policy runs nothing without it - so a script inside a chapter, if the
 * server's sanitiser ever let one through, still would not execute.
 */
object ReaderShell {
    private val random = SecureRandom()
    private val THEMES = setOf("light", "sepia", "dark", "black")

    fun assemble(
        template: String,
        locatorJs: String,
        readerJs: String,
        theme: String,
        nonce: String = nonce(),
        language: String? = null,
    ): String {
        // A script cannot contain its own closing tag and stay one script.
        require("</script" !in locatorJs.lowercase() && "</script" !in readerJs.lowercase()) {
            "a reader script contains </script>"
        }
        // One of the page's own four, never anything that could leave the
        // attribute it goes into.
        require(theme in THEMES) { "not a reader theme: $theme" }
        // One pass over the template, so what goes in is never searched again:
        // a script that mentions {{THEME}} in a comment, or one day holds
        // "{{READER_JS}}" in a string, stays exactly as written.
        return PLACEHOLDER.replace(template) {
            when (it.groupValues[1].ifEmpty { it.groupValues[2] }) {
                "NONCE" -> nonce
                // In the page from the start, so a dark reader never flashes white.
                "THEME" -> theme
                "LANG" -> lang(language)
                "LOCATOR_JS" -> locatorJs
                else -> readerJs
            }
        }
    }

    // The scripts' placeholders are JS comments, so the template is a valid
    // page before it is filled; the comment goes along with the name.
    private val PLACEHOLDER =
        Regex("""\{\{(NONCE|THEME|LANG)\}\}|/\*\{\{(LOCATOR_JS|READER_JS)\}\}\*/""")

    /**
     * The lang attribute for a language as the server has it: a BCP 47 tag
     * such as `en` or `pt-BR`, or `und` - "undetermined" - for none.
     *
     * It is a book's metadata, so it is not trusted into an attribute as it
     * stands. Something that is not shaped like a tag (`English`, a stray
     * quote) is `und` too: an honest "not known" reads better to a screen
     * reader than a wrong language, and the reader must open either way.
     */
    internal fun lang(language: String?): String {
        val tag = language?.trim()?.replace('_', '-')
        return if (tag != null && LANGUAGE_TAG.matches(tag)) tag else "und"
    }

    private val LANGUAGE_TAG = Regex("""[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8})*""")

    fun nonce(): String {
        val bytes = ByteArray(18)
        random.nextBytes(bytes)
        return Base64.getEncoder().encodeToString(bytes)
    }
}
