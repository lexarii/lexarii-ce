// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app.reader

import android.graphics.Bitmap
import android.graphics.Color as AndroidColor
import android.graphics.pdf.PdfRenderer
import android.os.ParcelFileDescriptor
import androidx.activity.compose.BackHandler
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.gestures.awaitEachGesture
import androidx.compose.foundation.gestures.awaitFirstDown
import androidx.compose.foundation.gestures.calculatePan
import androidx.compose.foundation.gestures.calculateZoom
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.Button
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.unit.dp
import com.lexarii.app.api.TOCEntry
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.ExecutorCoroutineDispatcher
import kotlinx.coroutines.asCoroutineDispatcher
import kotlinx.coroutines.delay
import kotlinx.coroutines.withContext
import java.io.File
import java.util.concurrent.Executors
import kotlin.math.abs
import kotlin.math.min
import kotlin.math.sqrt

// Long enough to read a sentence, short enough not to sit over the page.
private const val PDF_NOTICE_MS = 4_000L

/**
 * A PDF, a page at a time, drawn by the platform's own renderer - no library,
 * and nothing of the book leaves the app's private storage to be drawn.
 *
 * A tap on the left third goes back a page, the right third forward, the
 * middle shows the controls - the same as the EPUB reader. A swipe turns too;
 * two fingers zoom, and a zoomed page pans instead of turning.
 */
@Composable
fun PdfReaderScreen(
    controller: PdfController,
    settings: ReadingSettings,
    onSettings: (ReadingSettings) -> Unit,
    onClose: () -> Unit,
) {
    val systemDark = isSystemInDarkTheme()
    val palette = palette(settings.pageTheme(systemDark))
    SystemBarsFor(palette, systemDark)
    ReaderTheme(palette) {
        PdfContent(controller, settings, palette, onSettings, onClose)
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun PdfContent(
    controller: PdfController,
    settings: ReadingSettings,
    palette: PagePalette,
    onSettings: (ReadingSettings) -> Unit,
    onClose: () -> Unit,
) {
    val state by controller.state.collectAsState()
    val toc by controller.toc.collectAsState()
    val turning by controller.turning.collectAsState()
    var controls by rememberSaveable { mutableStateOf(false) }
    var typography by rememberSaveable { mutableStateOf(false) }
    var contents by rememberSaveable { mutableStateOf(false) }
    val pages = remember { PdfPages() }
    DisposableEffect(pages) { onDispose { pages.close() } }
    BackHandler { onClose() }

    Box(modifier = Modifier.fillMaxSize().background(Color(palette.page)).safeDrawingPadding()) {
        when (val current = state) {
            is PdfState.Opening -> Centred { Text("Opening…", style = MaterialTheme.typography.bodyLarge) }
            is PdfState.Failed -> Centred {
                Text(current.message, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyLarge)
                Button(onClick = { controller.retry() }, modifier = Modifier.fillMaxWidth()) { Text("Try again") }
                TextButton(onClick = onClose, modifier = Modifier.fillMaxWidth()) { Text("Back to the library") }
            }
            is PdfState.Showing -> PdfPage(current, pages, controller) { controls = !controls }
        }

        val showing = state as? PdfState.Showing
        ReaderControls(
            shown = controls,
            onToggle = { controls = !controls },
            onBack = onClose,
            onSettings = { typography = true },
            modifier = Modifier.align(Alignment.TopCenter),
        ) {
            if (toc.isNotEmpty()) TextButton(onClick = { contents = true }) { Text("Contents") }
            if (showing != null) {
                val of = if (showing.pageCount > 0) " of ${showing.pageCount}" else ""
                Text("${turning ?: showing.page}$of", style = MaterialTheme.typography.labelLarge, modifier = Modifier.padding(horizontal = 8.dp))
            }
        }

        turning?.let {
            Text(
                "Page $it…",
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.align(Alignment.BottomCenter).padding(bottom = 4.dp),
            )
        }

        val notice by controller.notice.collectAsState()
        notice?.let { message ->
            LaunchedEffect(message) {
                delay(PDF_NOTICE_MS)
                controller.noticeSeen()
            }
            Text(
                message,
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.inverseOnSurface,
                modifier = Modifier
                    .align(Alignment.Center)
                    .padding(24.dp)
                    .background(MaterialTheme.colorScheme.inverseSurface, MaterialTheme.shapes.small)
                    .padding(horizontal = 16.dp, vertical = 10.dp),
            )
        }

        if (typography) {
            TypographySheet(settings, text = false, onChange = onSettings, onDismiss = { typography = false })
        }
        if (contents) {
            ModalBottomSheet(onDismissRequest = { contents = false }) {
                LazyColumn(modifier = Modifier.fillMaxWidth().padding(bottom = 24.dp)) {
                    items(flatten(toc)) { (entry, depth) ->
                        // An entry the server could not resolve to a page points
                        // nowhere, and is listed without pretending otherwise.
                        val page = entry.spineIndex.takeIf { it >= 1 }
                        Text(
                            entry.title,
                            style = MaterialTheme.typography.bodyLarge,
                            color = if (page != null) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.onSurfaceVariant,
                            modifier = Modifier
                                .fillMaxWidth()
                                .clickable(enabled = page != null) {
                                    contents = false
                                    controls = false
                                    page?.let(controller::goTo)
                                }
                                .heightIn(min = 48.dp)
                                .padding(start = (24 + depth * 16).dp, end = 24.dp, top = 12.dp, bottom = 12.dp),
                        )
                    }
                }
            }
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

private fun flatten(entries: List<TOCEntry>, depth: Int = 0): List<Pair<TOCEntry, Int>> =
    entries.flatMap { listOf(it to depth) + flatten(it.children.orEmpty(), depth + 1) }

@Composable
private fun PdfPage(showing: PdfState.Showing, pages: PdfPages, controller: PdfController, onMiddle: () -> Unit) {
    BoxWithConstraints(modifier = Modifier.fillMaxSize()) {
        val width = constraints.maxWidth
        val height = constraints.maxHeight
        var zoom by remember(showing.page) { mutableFloatStateOf(1f) }
        var pan by remember(showing.page) { mutableStateOf(Offset.Zero) }
        // How sharply the page is drawn: at the fitted size to begin with,
        // and again at the zoom once a pinch has settled.
        var sharpness by remember(showing.page) { mutableFloatStateOf(1f) }
        // The last page drawn stays up until the next is ready, so turning
        // never flashes an empty screen.
        var image by remember { mutableStateOf<ImageBitmap?>(null) }
        var broken by remember { mutableStateOf<String?>(null) }

        LaunchedEffect(showing.file, showing.index, width, height, sharpness) {
            try {
                val (bitmap, count) = pages.render(showing.file, showing.index, width, height, sharpness)
                image = bitmap.asImageBitmap()
                broken = null
                controller.documentPages(count)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                image = null
                broken = e.message ?: "This page could not be shown"
            }
        }
        LaunchedEffect(zoom) {
            delay(250)
            sharpness = min(zoom, MAX_SHARPNESS)
        }

        // At the zoom as it is when asked, not as it was when the gesture began.
        fun clamp(o: Offset): Offset {
            val x = (zoom - 1) * width / 2
            val y = (zoom - 1) * height / 2
            return Offset(o.x.coerceIn(-x, x), o.y.coerceIn(-y, y))
        }

        Box(
            modifier = Modifier
                .fillMaxSize()
                .pointerInput(showing.page) {
                    val slop = viewConfiguration.touchSlop
                    awaitEachGesture {
                        val down = awaitFirstDown(requireUnconsumed = false)
                        var moved = Offset.Zero
                        var dragging = false
                        var pinched = false
                        do {
                            val event = awaitPointerEvent()
                            val pressed = event.changes.count { it.pressed }
                            if (pressed >= 2) {
                                pinched = true
                                zoom = (zoom * event.calculateZoom()).coerceIn(1f, MAX_ZOOM)
                                pan = clamp(pan + event.calculatePan())
                                event.changes.forEach { it.consume() }
                            } else if (pressed == 1) {
                                val delta = event.calculatePan()
                                moved += delta
                                if (!dragging && moved.getDistance() > slop) dragging = true
                                if (dragging) {
                                    if (zoom > 1f) pan = clamp(pan + delta)
                                    event.changes.forEach { it.consume() }
                                }
                            }
                        } while (event.changes.any { it.pressed })

                        if (zoom < 1.05f) {
                            zoom = 1f
                            pan = Offset.Zero
                        }
                        if (pinched) return@awaitEachGesture
                        if (!dragging) {
                            val x = down.position.x
                            when {
                                x < width / 3f -> controller.turn(-1)
                                x > width * 2f / 3f -> controller.turn(1)
                                else -> onMiddle()
                            }
                        } else if (zoom == 1f && abs(moved.x) > SWIPE_PX && abs(moved.x) > abs(moved.y) * 1.5f) {
                            controller.turn(if (moved.x < 0) 1 else -1)
                        }
                    }
                },
            contentAlignment = Alignment.Center,
        ) {
            val shown = image
            if (shown != null) {
                Image(
                    shown,
                    contentDescription = "Page ${showing.page}",
                    contentScale = ContentScale.Fit,
                    modifier = Modifier
                        .fillMaxSize()
                        .graphicsLayer {
                            scaleX = zoom
                            scaleY = zoom
                            translationX = pan.x
                            translationY = pan.y
                        },
                )
            }
            broken?.let {
                Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyLarge, modifier = Modifier.padding(24.dp))
            }
        }
    }
}

private const val MAX_ZOOM = 4f
private const val MAX_SHARPNESS = 3f
private const val SWIPE_PX = 120f

/**
 * The platform's PDF renderer, used from one thread of its own: it is not
 * safe to share, and only one page of a document may be open at a time.
 *
 * A kept book stays open between pages; a page from the server is its own
 * one-page document, opened for as long as it takes to draw.
 */
private class PdfPages {
    private val thread: ExecutorCoroutineDispatcher = Executors.newSingleThreadExecutor().asCoroutineDispatcher()
    private var file: File? = null
    private var descriptor: ParcelFileDescriptor? = null
    private var renderer: PdfRenderer? = null

    /** Page `index` of `file`, fitted into width x height and drawn `sharpness` times over; and how many pages the file has. */
    suspend fun render(file: File, index: Int, width: Int, height: Int, sharpness: Float): Pair<Bitmap, Int> = withContext(thread) {
        if (this@PdfPages.file != file) {
            shut()
            val fd = ParcelFileDescriptor.open(file, ParcelFileDescriptor.MODE_READ_ONLY)
            descriptor = fd
            renderer = try {
                PdfRenderer(fd)
            } catch (e: Exception) {
                fd.close()
                descriptor = null
                throw IllegalStateException("This PDF could not be opened")
            }
            this@PdfPages.file = file
        }
        val pdf = renderer ?: throw IllegalStateException("This PDF could not be opened")
        if (index !in 0 until pdf.pageCount) throw IllegalStateException("There is no page ${index + 1} in this file")
        val page = pdf.openPage(index)
        try {
            val fit = min(width / page.width.toFloat(), height / page.height.toFloat())
            var w = (page.width * fit * sharpness).coerceAtLeast(1f)
            var h = (page.height * fit * sharpness).coerceAtLeast(1f)
            // A page drawn sharp for a deep zoom is still bounded: a bitmap
            // this size is tens of megabytes, and past it is a crash.
            if (w * h > MAX_PIXELS) {
                val f = sqrt(MAX_PIXELS / (w * h))
                w *= f
                h *= f
            }
            val bitmap = Bitmap.createBitmap(w.toInt(), h.toInt(), Bitmap.Config.ARGB_8888)
            // A PDF page is transparent where nothing is printed: paper first.
            bitmap.eraseColor(AndroidColor.WHITE)
            page.render(bitmap, null, null, PdfRenderer.Page.RENDER_MODE_FOR_DISPLAY)
            bitmap to pdf.pageCount
        } finally {
            page.close()
        }
    }

    fun close() {
        thread.executor.execute { shut() }
        thread.close()
    }

    private fun shut() {
        runCatching { renderer?.close() }
        runCatching { descriptor?.close() }
        renderer = null
        descriptor = null
        file = null
    }

    private companion object {
        const val MAX_PIXELS = 12_000_000f
    }
}
