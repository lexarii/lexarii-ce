// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

/**
 * The real files from assets/, not a stand-in template: the claim worth
 * testing is that the page the app ships allows its own two scripts and no
 * others.
 */
class ReaderShellTest {
    private val assets = File("src/main/assets/reader")
    private fun read(name: String) = File(assets, name).readText()

    private fun shell(nonce: String = "n0nce", theme: String = "light", language: String? = null) =
        ReaderShell.assemble(read("reader.html"), read("locator.js"), read("reader.js"), theme, nonce, language)

    @Test
    fun `every script carries the nonce the policy names`() {
        val page = shell()
        assertTrue(page.contains("script-src 'nonce-n0nce'"))
        // The page's own text, not the scripts', which may mention a
        // placeholder in a comment.
        val outsideScripts = page.replace(Regex("(?s)(<script[^>]*>).*?</script>"), "$1</script>")
        assertFalse("a placeholder survived", outsideScripts.contains("{{"))
        val scripts = Regex("<script([^>]*)>").findAll(page).map { it.groupValues[1] }.toList()
        assertTrue("both scripts inlined", scripts.size == 2)
        for (attributes in scripts) assertTrue(attributes, attributes.contains("nonce=\"n0nce\""))
    }

    /** 'unsafe-inline' would let a script in a chapter run, which is the thing the nonce is for. */
    @Test
    fun `the policy allows no inline script and nothing from anywhere else`() {
        val policy = Regex("""Content-Security-Policy"\s+content="([^"]*)"""").find(shell())!!.groupValues[1]
        assertFalse(policy.contains("unsafe-inline"))
        assertFalse(policy.contains("unsafe-eval"))
        assertTrue(policy.contains("default-src 'none'"))
    }

    @Test
    fun `the locator module is the compiled web one`() {
        assertTrue(read("locator.js").contains("GENERATED from web/src/lib/reader/locator.ts"))
        assertTrue(shell().contains("var Locator ="))
    }

    @Test
    fun `each load gets its own nonce`() {
        assertNotEquals(ReaderShell.nonce(), ReaderShell.nonce())
    }

    @Test
    fun `the chosen theme is in the page from the first frame`() {
        for (theme in listOf("light", "sepia", "dark", "black")) {
            assertTrue(theme, shell(theme = theme).contains(" data-theme=\"$theme\">"))
        }
    }

    /** It goes into an attribute; only the page's own four names ever do. */
    @Test(expected = IllegalArgumentException::class)
    fun `a theme that is not one of the four is refused`() {
        shell(theme = "dark\"><script>")
    }

    /** Inserted once and never searched again: a script is exactly what was written. */
    @Test
    fun `the scripts go in verbatim, whatever they mention`() {
        val locator = "var a = '/*{{READER_JS}}*/ {{LANG}}'; var b = '$1 {{NONCE}}';"
        val reader = "/* the page is {{THEME}} */"
        val page = ReaderShell.assemble(read("reader.html"), locator, reader, "sepia", "n0nce")
        assertTrue(page.contains(">$locator</script>"))
        assertTrue(page.contains(">$reader</script>"))
    }

    /** Filled with the scripts themselves, not left inside the comments that hold their places. */
    @Test
    fun `each script is exactly its file, with nothing of the placeholder left`() {
        val page = shell()
        assertTrue(page.contains(">${read("locator.js")}</script>"))
        assertTrue(page.contains(">${read("reader.js")}</script>"))
        assertFalse(page.contains("LOCATOR_JS"))
        assertFalse(page.contains("READER_JS"))
    }

    @Test
    fun `the page has a title and says what language its text is in`() {
        val page = shell(language = "pt-BR")
        assertTrue(page.contains("<title>Lexarii</title>"))
        assertTrue(page.contains("<html lang=\"pt-BR\" data-theme=\"light\">"))
        // Not known is said as not known, rather than left out or guessed.
        assertTrue(shell().contains("<html lang=\"und\""))
    }

    /** It is a book's metadata going into an attribute: only something shaped like a tag gets there. */
    @Test
    fun `a language that is not a language tag is undetermined`() {
        assertEquals("en", ReaderShell.lang("en"))
        assertEquals("en-GB", ReaderShell.lang(" en_GB "))
        assertEquals("zh-Hant-TW", ReaderShell.lang("zh-Hant-TW"))
        for (bad in listOf(null, "", "e", "English (UK)", "en\" onload=\"x", "en-", "-en", "en--GB", "fr-toolongsubtag")) {
            assertEquals(bad.toString(), "und", ReaderShell.lang(bad))
        }
        assertFalse(shell(language = "en\"><script>").contains("<script>\""))
    }

    @Test(expected = IllegalArgumentException::class)
    fun `a script that could close its own tag is refused`() {
        ReaderShell.assemble(read("reader.html"), "var a = '</script>'", read("reader.js"), "light")
    }
}
