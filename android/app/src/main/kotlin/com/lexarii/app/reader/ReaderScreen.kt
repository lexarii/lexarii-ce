// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import android.annotation.SuppressLint
import android.content.Context
import android.graphics.Rect
import android.view.ActionMode
import android.view.Menu
import android.view.MenuItem
import android.view.View
import android.view.ViewGroup
import android.webkit.JavascriptInterface
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.activity.compose.BackHandler
import androidx.compose.foundation.background
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.SideEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.snapshotFlow
import androidx.compose.runtime.saveable.listSaver
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.graphics.Color as ComposeColor
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import com.lexarii.app.offline.Delivery
import com.lexarii.app.offline.Mark
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import java.io.ByteArrayInputStream
import java.net.URI
import kotlin.math.roundToInt

// Long enough to read a sentence, short enough not to sit over the page.
private const val NOTICE_MS = 4_000L

@Composable
fun ReaderScreen(
    controller: ReaderController,
    server: String,
    settings: ReadingSettings,
    onSettings: (ReadingSettings) -> Unit,
    onClose: () -> Unit,
) {
    val systemDark = isSystemInDarkTheme()
    val theme = settings.pageTheme(systemDark)
    val palette = palette(theme)
    SystemBarsFor(palette, systemDark)
    ReaderTheme(palette) {
        ReaderContent(controller, server, settings, systemDark, theme, palette, onSettings, onClose)
    }
}

@Composable
private fun ReaderContent(
    controller: ReaderController,
    server: String,
    settings: ReadingSettings,
    systemDark: Boolean,
    theme: String,
    palette: PagePalette,
    onSettings: (ReadingSettings) -> Unit,
    onClose: () -> Unit,
) {
    val state by controller.state.collectAsState()
    val marks by controller.marks.collectAsState()
    val open by controller.openMark.collectAsState()
    val unplaced by controller.unplaced.collectAsState()
    var controls by rememberSaveable { mutableStateOf(false) }
    var typography by rememberSaveable { mutableStateOf(false) }
    // The page's back goes out through it; the controls' back does the same.
    var leave by remember { mutableStateOf(onClose) }

    Box(modifier = Modifier.fillMaxSize().background(ComposeColor(palette.page)).safeDrawingPadding()) {
        ReaderPage(
            controller = controller,
            server = server,
            theme = theme,
            page = palette.page,
            typography = typographyCall(settings, systemDark),
            onChrome = { controls = !controls },
            onLeave = { leave = it },
            onClose = onClose,
        )

        ReaderControls(
            shown = controls,
            onToggle = { controls = !controls },
            onBack = { leave() },
            onSettings = { typography = true },
            modifier = Modifier.align(Alignment.TopCenter),
        )
        if (typography) {
            TypographySheet(settings, text = true, onChange = onSettings, onDismiss = { typography = false })
        }

        val notice by controller.notice.collectAsState()
        notice?.let { message ->
            LaunchedEffect(message) {
                delay(NOTICE_MS)
                controller.noticeSeen()
            }
            Text(
                message,
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.inverseOnSurface,
                modifier = Modifier
                    .align(Alignment.TopCenter)
                    .padding(top = 8.dp)
                    .background(MaterialTheme.colorScheme.inverseSurface, MaterialTheme.shapes.small)
                    .padding(horizontal = 16.dp, vertical = 10.dp),
            )
        }

        if (unplaced > 0 && state is ReaderState.Showing) {
            // Quiet, and in the margin: the marks are kept, and the web shows
            // them as orphaned. This only says why a highlight is missing.
            Text(
                if (unplaced == 1) "1 highlight here could not be placed" else "$unplaced highlights here could not be placed",
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.align(Alignment.BottomCenter),
            )
        }

        open?.let { opened ->
            marks.firstOrNull { it.id == opened.id }?.let { MarkSheet(it, opened.writing, controller) }
        }

        val lost by controller.lostNote.collectAsState()
        lost?.let { LostNoteDialog(it, controller) }

        when (val current = state) {
            is ReaderState.Opening -> Centred { Text("Opening…", style = MaterialTheme.typography.bodyLarge) }
            is ReaderState.Unsupported -> Centred {
                Text(
                    "Reading ${current.format.uppercase()} files is not in the app yet. " +
                        "It opens in the browser reader.",
                    style = MaterialTheme.typography.bodyLarge,
                )
                TextButton(onClick = onClose) { Text("Back to the library") }
            }
            is ReaderState.Failed -> Centred {
                Text(current.message, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyLarge)
                Button(onClick = { controller.retry() }, modifier = Modifier.fillMaxWidth()) { Text("Try again") }
                TextButton(onClick = onClose, modifier = Modifier.fillMaxWidth()) { Text("Back to the library") }
            }
            is ReaderState.Showing -> Unit
        }
    }
}

@Composable
private fun Centred(content: @Composable () -> Unit) {
    Column(
        modifier = Modifier.fillMaxSize().padding(24.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp, Alignment.CenterVertically),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) { content() }
}

@Composable
private fun ReaderPage(
    controller: ReaderController,
    server: String,
    theme: String,
    page: Int,
    typography: String,
    onChrome: () -> Unit,
    onLeave: (() -> Unit) -> Unit,
    onClose: () -> Unit,
) {
    // Whether the page's script has run and `Reader` exists. A new WebView -
    // after a rotation - starts false again.
    var ready by remember { mutableStateOf(false) }
    var view by remember { mutableStateOf<WebView?>(null) }
    val styled by rememberUpdatedState(typography)
    val chrome by rememberUpdatedState(onChrome)

    AndroidView(
        modifier = Modifier.fillMaxSize(),
        factory = { context ->
            readerWebView(context, controller, server, theme, page, onChrome = { chrome() }) {
                // Before `ready`, so the first chapter shown into this WebView
                // is already the one at the paragraph last reported.
                controller.reattached()
                ready = true
            }.also { view = it }
        },
        // Behind the page before it paints, and in the margins it leaves.
        update = { it.setBackgroundColor(page) },
    )

    // Out through the page, so the position it has not reported yet - the
    // report waits a moment after each turn - is reported before the reader
    // closes. The script's reply arrives after the report it made.
    val leave = {
        val web = view
        if (web != null && ready) web.evaluateJavascript("Reader.flush()") { onClose() } else onClose()
    }
    SideEffect { onLeave(leave) }
    BackHandler { leave() }

    LaunchedEffect(ready) {
        val web = view ?: return@LaunchedEffect
        if (!ready) return@LaunchedEffect
        // The typography before anything is shown, so the first chapter is
        // laid out once, in the reader's own type - then again whenever it
        // changes.
        var sent = styled
        web.evaluateJavascript(sent, null)
        launch {
            snapshotFlow { styled }.collect {
                if (it != sent) {
                    sent = it
                    web.evaluateJavascript(it, null)
                }
            }
        }
        launch {
            controller.state.collect { current ->
                if (current is ReaderState.Showing) {
                    web.evaluateJavascript(showCall(current), null)
                    web.evaluateJavascript(marksCall(controller.marks.value), null)
                }
            }
        }
        // All of this BookFile's marks, every time the set changes. The page
        // keeps them and paints the chapter's own after each show.
        controller.marks.collect { web.evaluateJavascript(marksCall(it), null) }
    }

    DisposableEffect(Unit) {
        onDispose { view?.destroy() }
    }
}

@SuppressLint("SetJavaScriptEnabled")
private fun readerWebView(
    context: Context,
    controller: ReaderController,
    server: String,
    theme: String,
    page: Int,
    onChrome: () -> Unit,
    onReady: () -> Unit,
): WebView {
    val web = ReaderWebView(context)
    // Without this the WebView gets wrap-content from Compose, which makes it
    // lay the page out as tall as its content - so every height: 100% in the
    // page is zero, the columns are zero tall, and a chapter renders as a
    // blank page. Found on the emulator; no browser shows it.
    web.layoutParams = ViewGroup.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT)
    web.setBackgroundColor(page)
    web.settings.apply {
        // The pagination and the locators are script. Everything else a
        // WebView can do is switched off: this one shows chapters and nothing
        // more, and a chapter is untrusted input.
        javaScriptEnabled = true
        javaScriptCanOpenWindowsAutomatically = false
        setSupportMultipleWindows(false)
        allowFileAccess = false
        allowContentAccess = false
        domStorageEnabled = false
        setGeolocationEnabled(false)
        cacheMode = WebSettings.LOAD_NO_CACHE
        // The phone's own text size, on purpose: somebody who made all text
        // larger in the system settings gets larger text here without being
        // asked twice, and the reader's size setting multiplies on top of it.
        // Set outright rather than left to the WebView's default, which has
        // not always followed the system on every version.
        textZoom = (context.resources.configuration.fontScale * 100).roundToInt().coerceIn(50, 300)
        // Safe Browsing sends what the WebView loads to Google. It only ever
        // loads this person's own server, which is exactly what should not
        // leave the device.
        safeBrowsingEnabled = false
    }

    val origin = URI(server.trimEnd('/'))
    val assets = "/api/v1/books/${controller.bookFileId}/assets/"

    web.webViewClient = object : WebViewClient() {
        // The page never navigates. Links are handled in its script; anything
        // that gets here anyway goes nowhere.
        override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest) = true

        /**
         * Every request the page makes lands here, and only this book's
         * images get an answer - fetched by the app, with its bearer token,
         * which the page itself never sees.
         */
        override fun shouldInterceptRequest(view: WebView, request: WebResourceRequest): WebResourceResponse? {
            val url = request.url
            // The page itself arrives as a data: URL, and recent WebViews ask
            // about it here too - refusing it refuses the whole reader. A
            // data: URL carries its content inside it and never reaches a
            // network, so it is let through; nothing else is.
            if (url.scheme == "data") return null
            val sameServer = url.scheme == origin.scheme && url.host == origin.host && url.port == origin.port
            val path = url.encodedPath
            if (request.method != "GET" || !sameServer || path == null || !path.startsWith(assets)) {
                return refused(403, "Forbidden")
            }
            return try {
                // Off the main thread already: the WebView calls this on its own.
                val asset = runBlocking(Dispatchers.IO) { controller.asset(path) }
                WebResourceResponse(asset.contentType.substringBefore(';').trim(), null, ByteArrayInputStream(asset.bytes))
            } catch (e: Exception) {
                refused(404, "Not Found")
            }
        }
    }

    web.addJavascriptInterface(Bridge(web, controller, onReady, onChrome), "LexariiBridge")

    // Highlight and Note from the selection menu: the page is asked for its
    // selection as locators, and the answer is checked before it is kept.
    web.onMark = { writing, done ->
        web.evaluateJavascript("Reader.highlightSelection()") { result ->
            controller.highlight(result, writing)
            done()
        }
    }

    val shell = ReaderShell.assemble(
        template = context.assets.open("reader/reader.html").bufferedReader().use { it.readText() },
        locatorJs = context.assets.open("reader/locator.js").bufferedReader().use { it.readText() },
        readerJs = context.assets.open("reader/reader.js").bufferedReader().use { it.readText() },
        theme = theme,
        // No language yet: the page is built before the manifest arrives, and
        // neither it nor the shelf entry carries the edition's language. The
        // page says "und", undetermined, until one of them does.
        language = null,
    )
    // The server as the base, so the image paths a chapter carries resolve to
    // it and arrive at shouldInterceptRequest above. No history: back is the
    // app's, not the page's.
    web.loadDataWithBaseURL("${origin.scheme}://${origin.rawAuthority}/", shell, "text/html", "utf-8", null)
    return web
}

private fun refused(status: Int, reason: String) =
    WebResourceResponse("text/plain", "utf-8", status, reason, emptyMap(), ByteArrayInputStream(ByteArray(0)))

/**
 * What the page may say to the app. Every call arrives on a WebView thread
 * and is moved to the main one, where the controller lives.
 */
private class Bridge(
    private val view: WebView,
    private val controller: ReaderController,
    private val onReady: () -> Unit,
    private val onChrome: () -> Unit,
) {
    @JavascriptInterface
    fun ready() {
        view.post(onReady)
    }

    /**
     * A tap in the middle of the page: show or hide the reader's controls.
     * It carries nothing, so there is nothing in it to check - the worst a
     * chapter could do with it is toggle a toolbar.
     */
    @JavascriptInterface
    fun chrome() {
        view.post(onChrome)
    }

    @JavascriptInterface
    fun position(locator: String, percent: Double) {
        view.post { controller.reportPosition(locator, percent) }
    }

    @JavascriptInterface
    fun edge(direction: Int) {
        view.post { controller.turnedOffEdge(direction) }
    }

    @JavascriptInterface
    fun follow(spine: Int, fragment: String) {
        view.post { controller.follow(spine, fragment) }
    }

    /** A painted mark was tapped. The controller opens it only if it is one it holds. */
    @JavascriptInterface
    fun markTapped(id: String) {
        view.post { controller.markTapped(id) }
    }

    /**
     * Marks the page could not find in the chapter it has just painted, as a
     * JSON array of ids, and which chapter that was. Sent after every paint,
     * an empty list included.
     */
    @JavascriptInterface
    fun unresolved(idsJson: String, spineIndex: Int) {
        view.post { controller.unresolved(idsJson, spineIndex) }
    }

    /** The same from a page that does not say which chapter: counted, not reported. */
    @JavascriptInterface
    fun unresolved(idsJson: String) {
        view.post { controller.unresolved(idsJson) }
    }
}

/**
 * The reader's WebView, with Highlight and Note in the menu that a selection
 * brings up.
 *
 * Chromium starts that menu - floating on every version this app runs on -
 * through the view it draws into, so this is where its callback can be
 * wrapped. Copy, Share and the rest stay; the two items are added beside them.
 */
@SuppressLint("ViewConstructor")
private class ReaderWebView(context: Context) : WebView(context) {
    /** Called with whether a note is wanted, and what ends the menu once the page has answered. */
    var onMark: ((writing: Boolean, done: () -> Unit) -> Unit)? = null

    override fun startActionMode(callback: ActionMode.Callback?, type: Int): ActionMode? =
        super.startActionMode(callback?.let { Marking(it) }, type)

    override fun startActionMode(callback: ActionMode.Callback?): ActionMode? =
        super.startActionMode(callback?.let { Marking(it) })

    /**
     * Chromium's own callback, with two items more.
     *
     * A Callback2, and passing on where the selection is, because a floating
     * menu is placed by that - without it the menu would float over the
     * middle of the page rather than beside the words.
     */
    private inner class Marking(private val inner: ActionMode.Callback) : ActionMode.Callback2() {
        override fun onCreateActionMode(mode: ActionMode, menu: Menu): Boolean {
            val shown = inner.onCreateActionMode(mode, menu)
            addItems(menu)
            return shown
        }

        override fun onPrepareActionMode(mode: ActionMode, menu: Menu): Boolean {
            val changed = inner.onPrepareActionMode(mode, menu)
            // Chromium may rebuild its menu here, and a rebuilt menu has lost
            // whatever was added to it.
            return addItems(menu) || changed
        }

        override fun onActionItemClicked(mode: ActionMode, item: MenuItem): Boolean {
            val writing = when (item.itemId) {
                HIGHLIGHT -> false
                NOTE -> true
                else -> return inner.onActionItemClicked(mode, item)
            }
            // The menu stays until the page has answered: ending it clears
            // the selection, and the selection is what is being asked for.
            onMark?.invoke(writing) { mode.finish() } ?: mode.finish()
            return true
        }

        override fun onDestroyActionMode(mode: ActionMode) = inner.onDestroyActionMode(mode)

        override fun onGetContentRect(mode: ActionMode, view: View, outRect: Rect) {
            if (inner is ActionMode.Callback2) inner.onGetContentRect(mode, view, outRect) else super.onGetContentRect(mode, view, outRect)
        }
    }

    private fun addItems(menu: Menu): Boolean {
        if (menu.findItem(HIGHLIGHT) != null) return false
        // Order 0, ahead of Chromium's own: marking is what a selection in a
        // reader is most often for.
        menu.add(Menu.NONE, HIGHLIGHT, 0, "Highlight")
        menu.add(Menu.NONE, NOTE, 0, "Note")
        return true
    }

    private companion object {
        // Any ids Chromium's own menu does not use.
        const val HIGHLIGHT = 0x534d_0001
        const val NOTE = 0x534d_0002
    }
}

/**
 * One mark, opened by a tap on it or to write the note just asked for.
 *
 * Somebody else's shared mark is shown with who wrote it and nothing that
 * edits it: a shared book is not a shared scribbling surface.
 *
 * Leaving the sheet any way at all - Save, back, a swipe down, a tap on the
 * page - keeps what was typed. A back press to hide the keyboard and a second
 * one a moment later is how people close things, and it must not cost them a
 * paragraph.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun MarkSheet(mark: Mark, writing: Boolean, controller: ReaderController) {
    val sheet = rememberModalBottomSheetState(skipPartiallyExpanded = true)
    var draft by rememberSaveable(mark.id, stateSaver = listSaver(save = { it.toSaved() }, restore = NoteDraft::fromSaved)) {
        mutableStateOf(NoteDraft(mark.note.orEmpty()))
    }
    // A pull or the per-book list can bring a newer note while the sheet is
    // open. An untouched draft follows it; a typed one is kept and flagged.
    LaunchedEffect(mark.note) { draft = draft.noteNow(mark.note.orEmpty()) }
    // Told as it changes, so a deletion from elsewhere can hand the words back.
    LaunchedEffect(draft) { controller.drafting(mark.id, draft.toSave()) }
    var confirming by rememberSaveable(mark.id) { mutableStateOf(false) }
    val leave = {
        val typed = draft.toSave()
        if (mark.shared || typed == null) controller.closeMark() else controller.updateNote(mark.id, typed)
    }

    ModalBottomSheet(onDismissRequest = leave, sheetState = sheet) {
        Column(
            modifier = Modifier
                .fillMaxWidth()
                .verticalScroll(rememberScrollState())
                .padding(start = 24.dp, end = 24.dp, bottom = 24.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            mark.selectedText?.let {
                Text("\u201c$it\u201d", style = MaterialTheme.typography.bodyLarge, fontStyle = FontStyle.Italic)
            }
            if (mark.shared) {
                Text("by ${mark.author?.takeIf { it.isNotBlank() } ?: "somebody else"}", style = MaterialTheme.typography.labelLarge)
                mark.note?.takeIf { it.isNotBlank() }?.let { Text(it, style = MaterialTheme.typography.bodyMedium) }
                return@Column
            }

            val focus = remember { FocusRequester() }
            OutlinedTextField(
                value = draft.text,
                onValueChange = { draft = draft.edited(it.take(ReaderController.MAX_NOTE)) },
                label = { Text("Note") },
                minLines = 3,
                modifier = Modifier.fillMaxWidth().heightIn(max = 320.dp).focusRequester(focus),
            )
            if (writing) LaunchedEffect(mark.id) { focus.requestFocus() }

            if (draft.changedElsewhere) {
                Text(
                    "This note was changed on another device while you were writing. Saving keeps what you wrote.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.error,
                )
            }
            val clearable = draft.text.isNotBlank() || controller.canClearNote(mark)
            if (!clearable) {
                Text(
                    "A note here needs some words. To remove it, delete it.",
                    style = MaterialTheme.typography.bodySmall,
                )
            }
            when (mark.delivery) {
                Delivery.REFUSED -> Text(
                    "The server did not accept this. It is kept on this device.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.error,
                )
                Delivery.UNSENT -> Text("Not sent to the server yet.", style = MaterialTheme.typography.bodySmall)
                Delivery.SENT -> Unit
            }

            Row(verticalAlignment = Alignment.CenterVertically) {
                TextButton(
                    onClick = { confirming = true },
                    colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
                ) { Text("Delete") }
                Spacer(Modifier.weight(1f))
                Button(
                    onClick = { controller.updateNote(mark.id, draft.text) },
                    enabled = draft.typed && draft.text.trim() != mark.note.orEmpty() && clearable,
                ) { Text("Save") }
            }
        }
    }

    // Asked, because it is one tap from Save and takes the note with it on
    // every device this person reads on.
    if (confirming) {
        val what = if (mark.hasNote || draft.text.isNotBlank()) "note" else "highlight"
        AlertDialog(
            onDismissRequest = { confirming = false },
            title = { Text("Delete this $what?") },
            text = { Text("It is removed from every device you read on.") },
            confirmButton = {
                TextButton(
                    onClick = {
                        confirming = false
                        controller.delete(mark.id)
                    },
                    colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
                ) { Text("Delete") }
            },
            dismissButton = { TextButton(onClick = { confirming = false }) { Text("Keep") } },
        )
    }
}

/**
 * The words somebody was writing when the note was deleted on another device.
 * The deletion stands; the words are offered back - as a new note on the same
 * passage, or selectable here to copy - and only let go when they say so.
 */
@Composable
private fun LostNoteDialog(lost: LostNote, controller: ReaderController) {
    AlertDialog(
        // Not dismissed by a stray tap outside: that would be losing the words
        // the dialog exists to keep.
        onDismissRequest = {},
        title = { Text("Deleted on another device") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("You were writing this note when it was deleted elsewhere. What you wrote:")
                SelectionContainer {
                    Text(
                        lost.text,
                        style = MaterialTheme.typography.bodyMedium,
                        modifier = Modifier.heightIn(max = 240.dp).verticalScroll(rememberScrollState()),
                    )
                }
            }
        },
        confirmButton = { TextButton(onClick = { controller.keepLostNote() }) { Text("Save as a new note") } },
        dismissButton = { TextButton(onClick = { controller.dropLostNote() }) { Text("Discard") } },
    )
}
